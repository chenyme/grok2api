
(() => {
  const IS_SPA = window.__GROK_ADMIN_SPA__ === true;

  let batchActions = null;
  let isDragging = false;
  let startX = 0;
  let startY = 0;
  let initialLeft = 0;
  let initialTop = 0;
  let pointerDownHandler = null;

  function bindDraggable(target) {
    if (!target) return;
    if (batchActions && pointerDownHandler) {
      batchActions.removeEventListener("pointerdown", pointerDownHandler);
    }

    batchActions = target;
    batchActions.style.touchAction = "none";

    pointerDownHandler = (e) => {
      if (
        e.target.tagName.toLowerCase() === "button" ||
        e.target.closest("button")
      ) {
        return;
      }

      e.preventDefault();
      isDragging = true;
      batchActions.setPointerCapture(e.pointerId);
      startX = e.clientX;
      startY = e.clientY;

      const rect = batchActions.getBoundingClientRect();

      if (!batchActions.style.left || batchActions.style.left === "") {
        batchActions.style.left = rect.left + "px";
        batchActions.style.top = rect.top + "px";
        batchActions.style.transform = "none";
        batchActions.style.bottom = "auto";
      }

      initialLeft = parseFloat(batchActions.style.left);
      initialTop = parseFloat(batchActions.style.top);
      batchActions.classList.add("shadow-xl");
    };

    batchActions.addEventListener("pointerdown", pointerDownHandler);
  }

  function initBatchActionsDraggable() {
    const target = document.getElementById("batch-actions");
    if (!target) return;
    bindDraggable(target);
  }

  function resetBatchActionsDraggable() {
    if (batchActions && pointerDownHandler) {
      batchActions.removeEventListener("pointerdown", pointerDownHandler);
    }
    batchActions = null;
    pointerDownHandler = null;
    isDragging = false;
  }

  document.addEventListener("pointermove", (e) => {
    if (!isDragging || !batchActions) return;
    const dx = e.clientX - startX;
    const dy = e.clientY - startY;
    batchActions.style.left = `${initialLeft + dx}px`;
    batchActions.style.top = `${initialTop + dy}px`;
  });

  document.addEventListener("pointerup", (e) => {
    if (!isDragging || !batchActions) return;
    isDragging = false;
    batchActions.releasePointerCapture(e.pointerId);
    batchActions.classList.remove("shadow-xl");
  });

  window.initBatchActionsDraggable = initBatchActionsDraggable;
  window.resetBatchActionsDraggable = resetBatchActionsDraggable;

  if (!IS_SPA) {
    initBatchActionsDraggable();
  }
})();
