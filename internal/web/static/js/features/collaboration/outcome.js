import { isDirect } from "./strategy.js";
const object = (value) => !!value && typeof value === "object" && !Array.isArray(value);
const id = (value) => typeof value === "string" && /^[a-f0-9]{32}$/.test(value);
// Steps/turns use store.NewID (6 random bytes); run/message/artifact identities
// use NewCollaborationID (16 bytes). Never conflate the two namespaces.
const isStepID = (value) => typeof value === "string" && /^[a-f0-9]{12}$/.test(value);
const positive = (value) => Number.isSafeInteger(value) && Number(value) > 0;
const text = (value) => typeof value === "string" && !!value.trim() && !value.includes("\0");
// The server permits omitted/null empty slices in a valid typed handoff.
function strings(value) {
    if (value == null)
        return [];
    return Array.isArray(value) && value.length <= 32 && value.every(text) ? [...value] : undefined;
}
/** Select one confirmed delivery, never the last arbitrary model message.
 * Uses only durable state/message/step/artifact/verification events. Missing
 * pages or a view ahead of its event stream yield no result until they align.
 */
export function selectFinalOutcome(view, events) {
    const run = view.run, direct = isDirect(view);
    if (run.state !== "completed" || !id(run.id) || !positive(run.revision) || !positive(run.config_version) || (direct ? run.plan_version !== 0 : !positive(run.plan_version)))
        return;
    const current = (body) => body.config_version === run.config_version && body.plan_version === run.plan_version;
    let previous = 0, end = -1;
    for (let i = 0; i < events.length; i++) {
        const event = events[i];
        if (!positive(event.seq) || event.seq <= previous)
            return;
        previous = event.seq;
        if (event.type === "state" && object(event.body) && event.body.id === run.id && event.body.state === "completed" && event.body.revision === run.revision && current(event.body)) {
            end = i;
            break;
        }
    }
    if (end < 0)
        return;
    const confirmed = events.slice(0, end + 1), steps = new Map();
    const messages = [], artifacts = [], verifications = [];
    for (const event of confirmed) {
        if (!object(event.body))
            continue;
        const body = event.body, row = { seq: event.seq, body };
        if (event.type === "step" && isStepID(body.id) && body.run_id === run.id)
            steps.set(body.id, row);
        if (event.type === "message" && current(body) && id(body.id) && isStepID(body.step_id))
            messages.push(row);
        if (event.type === "artifact" && body.run_id === run.id)
            artifacts.push(row);
        if (event.type === "verification" && body.run_id === run.id && body.config_version === run.config_version)
            verifications.push(row);
    }
    const completedStep = (stepID, role) => {
        const step = steps.get(stepID);
        return step && current(step.body) && step.body.state === "completed" && step.body.role === role ? step : undefined;
    };
    const reviewRow = messages.findLast(row => row.body.type === "review" && row.body.sender_role === "coordinator");
    let approved = null;
    if (!direct) {
        if (!reviewRow || !object(reviewRow.body.body))
            return;
        const review = reviewRow.body.body, reviewStep = completedStep(String(reviewRow.body.step_id), "coordinator"), changes = strings(review.changes);
        if (review.decision !== "approve" || !text(review.summary) || !id(review.snapshot_id) || review.plan_version !== run.plan_version || !changes || changes.length || !reviewStep || !["review", "handoff_repair"].includes(String(reviewStep.body.kind)) || reviewStep.body.snapshot_id !== review.snapshot_id || reviewStep.seq <= reviewRow.seq)
            return;
        approved = { decision: "approve", summary: review.summary, snapshot_id: review.snapshot_id, plan_version: run.plan_version, changes };
    }
    else if (reviewRow)
        return;
    const upper = direct ? confirmed.at(-1).seq : reviewRow.seq;
    const resultRow = messages.findLast(row => row.seq < upper && row.body.type === "result" && row.body.sender_role === "executor");
    if (!resultRow || !object(resultRow.body.body))
        return;
    const result = resultRow.body.body, resultStep = completedStep(String(resultRow.body.step_id), "executor");
    const completed = strings(result.completed), remaining = strings(result.remaining), tests = strings(result.tests);
    if (!text(result.summary) || !completed || !remaining || direct && remaining.length > 0 || !tests || !resultStep || resultStep.seq <= resultRow.seq || resultStep.seq >= upper)
        return;
    const artifact = artifacts.findLast(row => direct ? (row.body.step_id === resultRow.body.step_id || resultStep.body.kind === "handoff_repair" && row.body.id === resultStep.body.snapshot_id) : row.body.id === approved.snapshot_id);
    const snapshotID = artifact?.body.id;
    if (!id(snapshotID))
        return;
    if (!artifact || artifact.body.kind !== "snapshot" || artifact.body.state !== "ready" || !isStepID(artifact.body.step_id) || artifact.seq >= resultRow.seq)
        return;
    if (resultStep.body.kind === "execute" || resultStep.body.kind === "repair") {
        // For a normal write, step.snapshot_id is the INPUT snapshot. The published
        // artifact, not that field, identifies the output approved by the reviewer.
        if (artifact.body.step_id !== resultRow.body.step_id)
            return;
    }
    else if (resultStep.body.kind === "handoff_repair") {
        // A format repair is read-only and reuses the original writer's artifact.
        const writer = completedStep(artifact.body.step_id, "executor");
        if (resultStep.body.snapshot_id !== snapshotID || !writer || !["execute", "repair"].includes(String(writer.body.kind)) || writer.seq >= resultRow.seq)
            return;
    }
    else
        return;
    const verificationRow = verifications.findLast(row => row.body.snapshot_id === snapshotID);
    if (!verificationRow || verificationRow.seq <= resultStep.seq || verificationRow.seq >= upper)
        return;
    const verification = verificationRow.body;
    if ((verification.state !== "completed" && verification.state !== "not_requested") || typeof verification.command !== "string" || typeof verification.output !== "string")
        return;
    if (verification.state === "not_requested" && verification.command !== "")
        return;
    if (verification.exit_code !== undefined && verification.exit_code !== null && (!Number.isSafeInteger(verification.exit_code) || verification.state === "completed" && verification.exit_code !== 0))
        return;
    if (verification.output_truncated !== undefined && typeof verification.output_truncated !== "boolean")
        return;
    if (verification.duration_ms !== undefined && (!Number.isSafeInteger(verification.duration_ms) || Number(verification.duration_ms) < 0))
        return;
    return {
        result: { summary: result.summary, completed, remaining, tests },
        review: approved,
        verification: { state: verification.state, command: verification.command, output: verification.output,
            ...(verification.exit_code !== undefined ? { exit_code: verification.exit_code } : {}),
            ...(typeof verification.output_truncated === "boolean" ? { output_truncated: verification.output_truncated } : {}),
            ...(typeof verification.duration_ms === "number" ? { duration_ms: verification.duration_ms } : {}) },
        snapshotID,
    };
}
