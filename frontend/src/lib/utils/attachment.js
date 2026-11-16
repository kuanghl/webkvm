/**
 * attachment.js — hand a server-side URL to the browser's download bar.
 *
 * The transfer results are cookie-authenticated attachments, so the
 * request has to be a plain navigation to that URL rather than a fetch:
 * the server streams the bytes with Content-Disposition and the browser
 * writes the file without the SPA ever holding it in memory.
 */
export function openAttachment(url) {
  if (!url) return;
  const a = document.createElement('a');
  a.href = url;
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
}
