export const MAX_CHAT_MESSAGE_BYTES = 8192;

const utf8Encoder = new TextEncoder();
const utf8Decoder = new TextDecoder();

function isASCII(value: string) {
  for (let index = 0; index < value.length; index += 1) {
    if (value.charCodeAt(index) > 0x7f) return false;
  }
  return true;
}

function hasControlOrBackslash(value: string) {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code <= 0x1f || code === 0x7f || code === 0x5c) return true;
  }
  return false;
}

export function capChatMessage(value: string, maxBytes = MAX_CHAT_MESSAGE_BYTES) {
  if (value.length <= maxBytes && isASCII(value)) return value;
  const encoded = utf8Encoder.encode(value);
  if (encoded.byteLength <= maxBytes) return value;
  let boundary = Math.max(0, maxBytes);
  while (boundary > 0 && (encoded[boundary] & 0xc0) === 0x80) boundary -= 1;
  return utf8Decoder.decode(encoded.subarray(0, boundary));
}

export function safeMarkdownUrl(url: string): string {
  const value = url.trim();
  if (!value || value.startsWith("//") || hasControlOrBackslash(value)) return "";
  if (/%(?:0[0-9a-f]|1[0-9a-f]|2f|5c|7f)/i.test(value)) return "";
  if (/^(https?:|mailto:)/i.test(value)) return value;
  if (/^(\/|\.\/|\.\.\/|#|\?)/.test(value)) {
    try {
      const origin = window.location.origin;
      const normalized = new URL(value, origin);
      if (normalized.origin !== origin) return "";
      return `${normalized.pathname}${normalized.search}${normalized.hash}`;
    } catch {
      return "";
    }
  }
  return "";
}