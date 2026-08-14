export async function copyToClipboard(text: string): Promise<boolean> {
  // The async Clipboard API is only reliable in a secure context. Some
  // browsers still expose navigator.clipboard over plain HTTP but reject the
  // write asynchronously; waiting for that rejection consumes the transient
  // user activation and makes the legacy fallback fail as well.
  if (globalThis.isSecureContext === true && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // The async Clipboard API can reject in a non-secure context (plain
      // HTTP on a LAN IP) or when the document is not focused. Fall through to
      // the synchronous execCommand fallback so copying still works there.
    }
  }
  return legacyCopy(text);
}

// execCommand("copy") is deprecated but remains the standard way to copy
// programmatically when the async Clipboard API is unavailable.
function legacyCopy(text: string): boolean {
  if (typeof document === "undefined" || !document.body) return false;
  const copyTarget = document.createElement("span");
  copyTarget.textContent = text;
  copyTarget.setAttribute("aria-hidden", "true");
  copyTarget.style.position = "fixed";
  copyTarget.style.top = "0";
  copyTarget.style.left = "0";
  copyTarget.style.width = "1px";
  copyTarget.style.height = "1px";
  copyTarget.style.overflow = "hidden";
  copyTarget.style.clipPath = "inset(50%)";
  copyTarget.style.whiteSpace = "pre";
  copyTarget.style.userSelect = "text";

  const activeElement = document.activeElement;
  const selection = document.getSelection();
  const savedRanges = selection
    ? Array.from({ length: selection.rangeCount }, (_, index) => selection.getRangeAt(index).cloneRange())
    : [];

  document.body.appendChild(copyTarget);
  const range = document.createRange();
  range.selectNodeContents(copyTarget);
  selection?.removeAllRanges();
  selection?.addRange(range);

  // Focus-trapped dialogs can steal focus from a hidden form control. Supply
  // the value on the copy event so the selected text is only a compatibility
  // fallback and cannot change what reaches the clipboard.
  let clipboardDataWritten = false;
  const handleCopy = (event: ClipboardEvent) => {
    if (!event.clipboardData) return;
    event.preventDefault();
    event.clipboardData.clearData();
    event.clipboardData.setData("text/plain", text);
    clipboardDataWritten = true;
  };
  document.addEventListener("copy", handleCopy, { capture: true, once: true });

  let commandSucceeded = false;
  try {
    commandSucceeded = document.execCommand("copy");
  } catch {
    // execCommand may throw in restricted environments.
  } finally {
    document.removeEventListener("copy", handleCopy, true);
    copyTarget.remove();
    selection?.removeAllRanges();
    for (const savedRange of savedRanges) selection?.addRange(savedRange);
    if (activeElement instanceof HTMLElement) activeElement.focus({ preventScroll: true });
  }
  return commandSucceeded && clipboardDataWritten;
}
