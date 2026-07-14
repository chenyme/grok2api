export async function copyToClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
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
  if (typeof document === "undefined") return false;
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.top = "0";
  textarea.style.left = "0";
  textarea.style.width = "1em";
  textarea.style.height = "1em";
  // A large font keeps iOS Safari from zooming the page and lets the
  // selection persist on the programmatically created element.
  textarea.style.fontSize = "2em";
  textarea.style.padding = "0";
  textarea.style.border = "none";
  textarea.style.outline = "none";
  textarea.style.boxShadow = "none";
  textarea.style.background = "transparent";

  const activeElement = document.activeElement as Element | null;
  const selection = document.getSelection();
  let savedRange: Range | null = null;
  if (selection && selection.rangeCount > 0) savedRange = selection.getRangeAt(0);

  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0, text.length);

  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    // execCommand may throw in restricted environments; ok stays false.
  }

  document.body.removeChild(textarea);
  if (savedRange && selection) {
    selection.removeAllRanges();
    selection.addRange(savedRange);
  }
  if (activeElement instanceof HTMLElement && typeof activeElement.focus === "function") {
    activeElement.focus();
  }
  return ok;
}
