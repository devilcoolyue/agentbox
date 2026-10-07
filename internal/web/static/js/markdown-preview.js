import { parse } from "../vendor/marked.esm.js";
import DOMPurify from "../vendor/purify.es.js";
/** File documents use GFM independently of the streaming chat renderer. */
export function markdownPreview(source, options) {
    const fragment = DOMPurify.sanitize(parse(source, { gfm: true, breaks: false, async: false }), {
        RETURN_DOM_FRAGMENT: true,
        ALLOWED_TAGS: ["a", "abbr", "b", "blockquote", "br", "code", "dd", "del", "details", "div", "dl", "dt", "em", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "img", "input", "ins", "kbd", "li", "ol", "p", "picture", "pre", "s", "samp", "source", "span", "strong", "sub", "summary", "sup", "table", "tbody", "td", "th", "thead", "tr", "ul"],
        ALLOWED_ATTR: ["href", "src", "srcset", "alt", "title", "width", "height", "align", "id", "name", "class", "media", "type", "checked", "disabled", "start", "colspan", "rowspan", "open"],
        ALLOW_DATA_ATTR: false, ALLOW_ARIA_ATTR: false,
    });
    const anchors = new Map(), used = new Set();
    // Strip application-facing classes/identifiers before attaching to the live DOM.
    for (const el of fragment.querySelectorAll("*")) {
        const language = el.tagName === "CODE" ? el.className.match(/(?:^|\s)language-([\w-]+)/)?.[1] : undefined;
        el.removeAttribute("class");
        const anchor = el.getAttribute("id") || el.getAttribute("name");
        el.removeAttribute("id");
        el.removeAttribute("name");
        if (anchor && !anchors.has(anchor))
            anchors.set(anchor, el);
        if (/^H[1-6]$/.test(el.tagName)) {
            const stem = (el.textContent || "").toLowerCase().replace(/[^\p{L}\p{N}\p{M}\s_-]/gu, "").replace(/\s/g, "-");
            let slug = stem, n = 0;
            while (used.has(slug))
                slug = stem + "-" + ++n;
            used.add(slug);
            if (!anchors.has(slug))
                anchors.set(slug, el);
        }
        if (el.hasAttribute("align") && !/^(left|center|right)$/i.test(el.getAttribute("align")))
            el.removeAttribute("align");
        for (const attr of ["width", "height", "colspan", "rowspan", "start"]) {
            if (el.hasAttribute(attr) && !/^\d{1,4}%?$/.test(el.getAttribute(attr)))
                el.removeAttribute(attr);
        }
        if (el.tagName === "INPUT") {
            if (el.getAttribute("type") !== "checkbox") {
                el.remove();
                continue;
            }
            el.disabled = true;
            el.parentElement?.classList.add("task-list-item");
        }
        if (language && el.parentElement?.tagName === "PRE" && (el.textContent?.length || 0) <= 100_000 && typeof Prism !== "undefined") {
            const grammar = Object.hasOwn(Prism.languages, language) ? Prism.languages[language] : undefined;
            if (grammar)
                el.innerHTML = Prism.highlight(el.textContent || "", grammar, language);
        }
    }
    for (const el of fragment.querySelectorAll("img, source")) {
        if (el.hasAttribute("src")) {
            const url = options.image(el.getAttribute("src"));
            if (url)
                el.setAttribute("src", url);
            else
                el.removeAttribute("src");
        }
        if (el.hasAttribute("srcset")) {
            const candidates = el.getAttribute("srcset").split(",").map(value => {
                const match = value.trim().match(/^(\S+)(?:\s+(\d+(?:\.\d+)?[wx]))?$/);
                const url = match && options.image(match[1]);
                return url ? url + (match[2] ? " " + match[2] : "") : "";
            });
            if (candidates.every(Boolean))
                el.setAttribute("srcset", candidates.join(", "));
            else
                el.removeAttribute("srcset");
        }
        if (el.tagName === "IMG") {
            el.setAttribute("loading", "lazy");
            el.setAttribute("referrerpolicy", "no-referrer");
        }
    }
    for (const link of fragment.querySelectorAll("a[href]")) {
        const href = link.getAttribute("href").trim();
        if (/^https?:\/\//i.test(href)) {
            link.target = "_blank";
            link.rel = "noopener noreferrer";
            continue;
        }
        link.removeAttribute("href");
        if (href.startsWith("#")) {
            let id;
            try {
                id = decodeURIComponent(href.slice(1));
            }
            catch {
                continue;
            }
            link.href = "#";
            link.addEventListener("click", event => { event.preventDefault(); anchors.get(id)?.scrollIntoView({ block: "start" }); });
        }
        else {
            const navigate = options.file(href);
            if (navigate) {
                link.href = "#";
                link.addEventListener("click", event => { event.preventDefault(); navigate(); });
            }
        }
    }
    return { content: fragment, scrollTo: (id) => anchors.get(id)?.scrollIntoView({ block: "start" }) };
}
