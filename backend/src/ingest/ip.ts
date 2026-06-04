// backend/src/ingest/ip.ts
type Headers = Record<string, string | undefined>;

/**
 * Strip the port from a CloudFront-Viewer-Address value.
 * CloudFront always formats this header as `<ip>:<port>`, even for IPv6
 * (which is sent unbracketed, e.g. `2001:db8::1:443`).  Cutting after the
 * last colon is therefore correct for both address families.
 * X-Forwarded-For entries carry no port and are NOT passed through this function.
 */
function stripPort(value: string): string {
  const i = value.lastIndexOf(':');
  return i === -1 ? value : value.slice(0, i);
}

export function extractClientIp(headers: Headers): string {
  const viewer = headers['cloudfront-viewer-address'];
  if (viewer) return stripPort(viewer.trim());

  const xff = headers['x-forwarded-for'];
  if (xff) return xff.split(',')[0].trim();

  return 'unknown';
}
