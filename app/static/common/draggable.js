
// Draggable Batch Actions
const batchActions = document.getElementById('batch-actions');
if (!batchActions) {
  // No toolbar on this page
} else {
let isDragging = false;
let startX, startY, initialLeft, initialTop;

batchActions.addEventListener('mousedown', (e) => {
  // Prevent dragging if clicking buttons
  if (e.target.tagName.toLowerCase() === 'button' || e.target.closest('button')) return;

  isDragging = true;
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

document.addEventListener('mousemove', (e) => {
  if (!isDragging) return;

  const dx = e.clientX - startX;
  const dy = e.clientY - startY;

  batchActions.style.left = `${initialLeft + dx}px`;
  batchActions.style.top = `${initialTop + dy}px`;
});

document.addEventListener('mouseup', () => {
  if (isDragging) {
    isDragging = false;
    batchActions.classList.remove('shadow-xl');
  }
});
}
