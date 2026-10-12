const record = (value) => !!value && typeof value === "object" && !Array.isArray(value);
export function providerEvent(value) {
    if (typeof value === "string")
        try {
            value = JSON.parse(value);
        }
        catch {
            return;
        }
    return record(value) ? value : undefined;
}
export function modelBlocks(value) {
    const event = providerEvent(value);
    if (!event)
        return [];
    const blocks = [];
    const add = (kind, text, key) => { if (typeof text === "string" && text)
        blocks.push({ kind, text, key: kind + ":" + (typeof key === "string" && key && key.length <= 256 ? key : text.slice(-64000)) }); };
    if (event.type === "assistant" && record(event.message) && Array.isArray(event.message.content)) {
        const message = event.message;
        for (const kind of ["text", "thinking"]) {
            const text = message.content.filter(record).filter(b => b.type === kind).map(b => b[kind]).filter((s) => typeof s === "string").join("\n");
            add(kind, text, message.id);
        }
    }
    else if (event.type === "item.completed" && record(event.item)) {
        if (event.item.type === "agent_message")
            add("text", event.item.text, event.item.id);
        else if (event.item.type === "reasoning")
            add("thinking", event.item.text, event.item.id);
    }
    else if (record(event.msg)) {
        if (event.msg.type === "agent_message")
            add("text", event.msg.message, event.msg.id);
        else if (event.msg.type === "agent_reasoning")
            add("thinking", event.msg.text, event.msg.id);
    }
    return blocks;
}
export function modelDelta(value) {
    const outer = providerEvent(value);
    if (!outer)
        return;
    const event = outer.type === "stream_event" ? providerEvent(outer.event) : outer;
    if (!event)
        return;
    if (event.type === "content_block_start" && record(event.content_block)) {
        const block = event.content_block;
        if (block.type === "text" || block.type === "thinking")
            return { kind: block.type, text: typeof block[block.type] === "string" ? block[block.type] : "", start: true };
    }
    if (event.type !== "content_block_delta" || !record(event.delta))
        return;
    const delta = event.delta;
    if (delta.type === "text_delta" && typeof delta.text === "string")
        return { kind: "text", text: delta.text, start: false };
    if (delta.type === "thinking_delta" && typeof delta.thinking === "string")
        return { kind: "thinking", text: delta.thinking, start: false };
}
