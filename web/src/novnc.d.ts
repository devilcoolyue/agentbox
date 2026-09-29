declare module "*vendor/novnc/core/rfb.js" {
 const RFB: new (target: HTMLElement, url: string) => EventTarget & {
  disconnect(): void; focus(): void; clipboardPasteFrom(text: string): void;
  sendKey(keysym: number, code: string, down?: boolean): void;
  scaleViewport: boolean; resizeSession: boolean; qualityLevel: number; compressionLevel: number;
 };
 export default RFB;
}
