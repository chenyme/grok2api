let fallbackSequence = 0;

export function createCreativeMessageId(): string {
  const randomUUID = globalThis.crypto?.randomUUID;
  if (typeof randomUUID === "function") {
    return randomUUID.call(globalThis.crypto);
  }

  const randomValues = new Uint32Array(2);
  if (typeof globalThis.crypto?.getRandomValues === "function") {
    globalThis.crypto.getRandomValues(randomValues);
  } else {
    randomValues[0] = Math.floor(Math.random() * 0xffffffff);
    randomValues[1] = Math.floor(Math.random() * 0xffffffff);
  }
  fallbackSequence = (fallbackSequence + 1) >>> 0;
  return `creative-${Date.now().toString(36)}-${randomValues[0].toString(36)}${randomValues[1].toString(36)}-${fallbackSequence.toString(36)}`;
}
