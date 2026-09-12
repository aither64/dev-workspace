import {initializeHighlighter, tokenizeSource} from "./highlight.js";

const cache = new Map();
const MAX_CACHE_BYTES = 24 * 1024 * 1024;
let cacheBytes = 0;

self.onmessage = async ({data}) => {
  const {id, text, language, warmup} = data;
  try {
    if (warmup) {
      await initializeHighlighter();
      self.postMessage({id, result: null});
      return;
    }
    if (typeof text !== "string" || typeof language !== "string") throw new Error("Invalid syntax request");
    const key = `${language}\0${text}`;
    let entry = cache.get(key);
    if (entry) {
      cache.delete(key);
      cache.set(key, entry);
    } else {
      const result = await tokenizeSource(text, language);
      const bytes = key.length * 2 + result.tokens.length * 32 + result.styles.length * 64;
      entry = {result, bytes};
      if (bytes <= MAX_CACHE_BYTES) {
        while (cache.size >= 32 || cacheBytes + bytes > MAX_CACHE_BYTES) {
          const first = cache.keys().next().value;
          cacheBytes -= cache.get(first).bytes;
          cache.delete(first);
        }
        cache.set(key, entry);
        cacheBytes += bytes;
      }
    }
    self.postMessage({id, result: entry.result});
  } catch {
    self.postMessage({id, error: "Syntax highlighting is unavailable."});
  }
};
