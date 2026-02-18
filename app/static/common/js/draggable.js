
// Draggable Batch Actions (Pointer Events for mouse + touch support)
const batchActions = document.getElementById('batch-actions');
if (!batchActions) {
  // No toolbar on this page
} else {
let isDragging = false;
let startX, startY, initialLeft, initialTop;

// Critical for mobile: prevents browser from interpreting drag as scroll
batchActions.style.touchAction = 'none';

batchActions.addEventListener('pointerdown', (e) => {
  // Prevent dragging if clicking buttons
  if (e.target.tagName.toLowerCase() === 'button' || e.target.closest('button')) return;

  e.preventDefault(); // Prevent text selection and implicit browser behaviors
  isDragging = true;
  batchActions.setPointerCapture(e.pointerId); // Track pointer even if it leaves the element
  startX = e.clientX;
  startY = e.clientY;

  const rect = batchActions.getBoundingClientRect();

  // Initialize top/left if not set (first time drag)
  if (!batchActions.style.left || batchActions.style.left === '') {
    batchActions.style.left = rect.left + 'px';
    batchActions.style.top = rect.top + 'px';
    // Remove transform to allow absolute positioning control
    batchActions.style.transform = 'none';
    batchActions.style.bottom = 'auto';
  }

  initialLeft = parseFloat(batchActions.style.left);
  initialTop = parseFloat(batchActions.style.top);

  // visual feedback
  batchActions.classList.add('shadow-xl');
});

document.addEventListener('pointermove', (e) => {
  if (!isDragging) return;

  const dx = e.clientX - startX;
  const dy = e.clientY - startY;

  batchActions.style.left = `${initialLeft + dx}px`;
  batchActions.style.top = `${initialTop + dy}px`;
});

document.addEventListener('pointerup', (e) => {
  if (isDragging) {
    isDragging = false;
    batchActions.releasePointerCapture(e.pointerId);
    batchActions.classList.remove('shadow-xl');
  }
});
}
