/* 委派相关的展示文案：任务类型、风险、状态、路由原因、主 Agent 评价。
 * 取值是服务端受控词表（config.DelegationTaskTypes / DelegationRisks），这里只管怎么说给人看。 */
import { t } from "../../i18n.js";
const TASK_TYPES = {
    search: "查找定位代码",
    summarize: "阅读并总结",
    mechanical_edit: "按规则批量修改",
    implement_spec: "按接口实现",
    write_tests: "补测试",
    fix_tests: "跑测试修失败",
    ui_copy: "文案与样式",
    docs: "文档",
    refactor_local: "模块内重构",
    other: "其他",
};
const RISKS = {
    auth: "鉴权",
    billing: "计费",
    migration: "数据迁移",
    concurrency: "并发",
    security: "安全",
    data_loss: "可能丢数据",
    public_api: "公开接口",
    cross_module: "跨模块",
    ambiguous_spec: "需求不明确",
};
const STATES = {
    routing: "选择跑腿中",
    running: "跑腿执行中",
    completed: "已完成",
    failed: "未完成",
    declined: "未委派，主 Agent 自己做",
    cancelled: "已取消",
    uncertain: "结果未知",
};
const FEEDBACK = {
    ok: "主 Agent 验收通过",
    minor_fix: "主 Agent 做了小修",
    redo: "主 Agent 判断需要重做",
    abandoned: "主 Agent 放弃了这个结果",
};
const REASONS = {
    "rules:only_eligible_worker": "按标签：只有这个跑腿能接这类任务",
    "rules:cheapest_eligible_worker": "按标签：能接的跑腿里选了最便宜的",
    "rules:no_eligible_worker": "按标签：没有能接这类任务或风险的跑腿",
    "rules:quota_exhausted": "额度已用完，不再委派",
    "continue:same_worker": "沿用上一次的跑腿接着改",
    "jev:worker": "Jev 判断适合交给跑腿",
    "jev:main_agent": "Jev 判断应由主 Agent 自己做",
};
export const taskTypeLabel = (v) => TASK_TYPES[v] ? t(TASK_TYPES[v]) : v;
export const riskLabel = (v) => RISKS[v] ? t(RISKS[v]) : v;
export const stateLabel = (v) => STATES[v] ? t(STATES[v]) : v;
export const feedbackLabel = (v) => FEEDBACK[v] ? t(FEEDBACK[v]) : v;
export const tierLabel = (v) => v === "standard" ? t("标准档") : t("经济档");
export function routeLabel(d) {
    const key = d.route ? `${d.route.source}:${d.route.reason}` : d.route_text || "";
    if (REASONS[key]) {
        const text = t(REASONS[key]);
        return d.route?.source === "jev" && d.route.confidence ? t("{text}（置信度 {p}）", { text, p: d.route.confidence.toFixed(2) }) : text;
    }
    if (d.route?.source === "fallback")
        return t("Jev 没有给出明确结论（{reason}），按标签选了最便宜的跑腿", { reason: d.route.reason || "-" });
    return key;
}
export function rejectionLabel(reason) {
    if (reason === "task_type")
        return t("不接这类任务");
    if (reason === "not_tagged")
        return t("未开启跑腿");
    if (reason.startsWith("risk:"))
        return t("不接「{risk}」风险", { risk: riskLabel(reason.slice(5)) });
    return reason;
}
export function durationLabel(ms) {
    if (!ms || ms < 0)
        return "";
    const s = Math.round(ms / 1000);
    if (s < 60)
        return t("{s} 秒", { s: String(s) });
    return t("{m} 分 {s} 秒", { m: String(Math.floor(s / 60)), s: String(s % 60) });
}
export const usdLabel = (micro) => "$" + (micro / 1e6).toFixed(micro && micro < 10000 ? 4 : 2);
