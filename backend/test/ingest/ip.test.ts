import { describe, it, expect } from 'vitest';
import { extractClientIp } from '../../src/ingest/ip';

describe('extractClientIp', () => {
  it('prefers CloudFront-Viewer-Address, stripping the port (IPv4)', () => {
    expect(extractClientIp({ 'cloudfront-viewer-address': '198.51.100.10:46532' }))
      .toBe('198.51.100.10');
  });
  it('handles IPv6 CloudFront-Viewer-Address (port after last colon)', () => {
    expect(extractClientIp({ 'cloudfront-viewer-address': '2001:db8::1:46532' }))
      .toBe('2001:db8::1');
  });
  it('falls back to the first X-Forwarded-For entry', () => {
    expect(extractClientIp({ 'x-forwarded-for': '203.0.113.7, 70.41.3.18, 150.172.238.178' }))
      .toBe('203.0.113.7');
  });
  it('returns "unknown" when no IP headers present', () => {
    expect(extractClientIp({})).toBe('unknown');
  });
});
