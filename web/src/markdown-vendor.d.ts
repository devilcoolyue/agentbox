declare module "*/vendor/marked.esm.js" {
  export function parse(source: string, options: {gfm: boolean; breaks: boolean; async: false}): string;
}
declare module "*/vendor/purify.es.js" {
  const purifier: {sanitize(source: string, options: Record<string, unknown>): DocumentFragment};
  export default purifier;
}
