import { yellow, green, magenta, label } from './colors.js';
import { truncateString } from '../utils/truncate.js';
const MAX_RECENT_COMPLETED = 2;
const MAX_AGENTS_SHOWN = 3;
export function renderAgentsLine(ctx) {
    const { agents } = ctx.transcript;
    const colors = ctx.config?.colors;
    const runningAgents = agents.filter((a) => a.status === 'running');
    const recentCompleted = agents
        .filter((a) => a.status === 'completed')
        .slice(-MAX_RECENT_COMPLETED);
    const seen = new Set();
    const toShow = [...runningAgents, ...recentCompleted]
        .filter((a) => {
        if (seen.has(a.id))
            return false;
        seen.add(a.id);
        return true;
    })
        .slice(-MAX_AGENTS_SHOWN);
    if (toShow.length === 0) {
        return null;
    }
    const lines = [];
    for (const agent of toShow) {
        lines.push(formatAgent(agent, colors));
    }
    return lines.join('\n');
}
function getStatusIcon(status) {
    switch (status) {
        case 'running':
            return yellow('◐');
        case 'completed':
        default:
            return green('✓');
    }
}
function formatAgent(agent, colors) {
    const statusIcon = getStatusIcon(agent.status);
    const type = magenta(agent.type);
    const model = agent.model ? label(`[${agent.model}]`, colors) : '';
    const desc = agent.description
        ? label(`: ${truncateString(agent.description, 40)}`, colors)
        : '';
    const elapsed = formatElapsed(agent);
    return `${statusIcon} ${type}${model ? ` ${model}` : ''}${desc} ${label(`(${elapsed})`, colors)}`;
}
function formatElapsed(agent) {
    const now = Date.now();
    const start = agent.startTime.getTime();
    const end = agent.endTime?.getTime() ?? now;
    const ms = Math.max(0, end - start);
    if (ms < 1000)
        return '<1s';
    if (ms < 60_000)
        return `${Math.round(ms / 1000)}s`;
    const totalSecs = Math.floor(ms / 1000);
    const mins = Math.floor(totalSecs / 60);
    const secs = totalSecs % 60;
    if (mins < 60)
        return `${mins}m ${secs}s`;
    const hours = Math.floor(mins / 60);
    const remainingMins = mins % 60;
    return `${hours}h ${remainingMins}m`;
}
//# sourceMappingURL=agents-line.js.map