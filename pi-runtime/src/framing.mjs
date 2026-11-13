// Private bounded protocol JSON, separate from the native history format.
export function jsonBytes(value, limit = 65536) {
  const check = (item) => {
    if (typeof item === 'string') { if (!item.isWellFormed()) throw new Error('invalid protocol UTF-8'); }
    else if (typeof item === 'number') { if (!Number.isFinite(item)) throw new Error('invalid protocol number'); }
    else if (item && typeof item === 'object') {
      for (const [key, child] of Object.entries(item)) { if (!key.isWellFormed()) throw new Error('invalid protocol key'); check(child); }
    } else if (item !== null && typeof item !== 'boolean') throw new Error('invalid protocol value');
  };
  check(value);
  const bytes = JSON.stringify(value);
  if (Buffer.byteLength(bytes) > limit) throw new Error('protocol body exceeds bound');
  return bytes;
}

export function parseRecord(text, allowed, limit = 1024 * 1024) {
  if (typeof text !== 'string' || !text.isWellFormed() || Buffer.byteLength(text) > limit) throw new Error('invalid protocol frame');
  let position = 0;
  const space = () => { while (/\s/.test(text[position] ?? '') && position < text.length) position++; };
  const string = () => {
    const start = position++;
    while (position < text.length) {
      if (text[position] === '\\') { position += 2; continue; }
      if (text[position++] === '"') { const value = JSON.parse(text.slice(start, position)); if (!value.isWellFormed()) throw new Error('invalid string'); return value; }
    }
    throw new Error('unfinished protocol string');
  };
  const value = () => {
    space(); const token = text[position];
    if (token === '"') { string(); return; }
    if (token === '{') {
      position++; space(); const keys = new Set();
      if (text[position] === '}') { position++; return; }
      while (true) {
        space(); if (text[position] !== '"') throw new Error('invalid object key');
        const key = string(); if (keys.has(key)) throw new Error('duplicate protocol key'); keys.add(key);
        space(); if (text[position++] !== ':') throw new Error('invalid object'); value(); space();
        const separator = text[position++]; if (separator === '}') return; if (separator !== ',') throw new Error('invalid object separator');
      }
    }
    if (token === '[') {
      position++; space(); if (text[position] === ']') { position++; return; }
      while (true) { value(); space(); const separator = text[position++]; if (separator === ']') return; if (separator !== ',') throw new Error('invalid array'); }
    }
    const match = /^(?:true|false|null|-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)/.exec(text.slice(position));
    if (!match) throw new Error('invalid protocol scalar'); position += match[0].length;
  };
  value(); space(); if (position !== text.length) throw new Error('trailing protocol value');
  const record = JSON.parse(text);
  if (!record || typeof record !== 'object' || Array.isArray(record) || Object.keys(record).some((key) => !allowed.includes(key))) throw new Error('unknown protocol fields');
  return record;
}
