export type AccountTaskProgressPhase = "importing" | "converting" | "syncing";

export type AccountTaskProgressDTO = {
  completed: number;
  total: number;
  phase?: AccountTaskProgressPhase;
};

export type AccountTaskProgressScheduler = {
  now: () => number;
  setTimeout: (callback: () => void, delay: number) => unknown;
  clearTimeout: (handle: unknown) => void;
};

type AccountTaskProgressControllerOptions = {
  onProgress?: (value: AccountTaskProgressDTO) => void;
  phases?: readonly AccountTaskProgressPhase[];
  intervalMs?: number;
  scheduler?: AccountTaskProgressScheduler;
};

export type AccountTaskProgressController = {
  report: (value: AccountTaskProgressDTO) => void;
  flush: () => void;
  dispose: () => void;
};

const browserScheduler: AccountTaskProgressScheduler = {
  now: () => performance.now(),
  setTimeout: (callback, delay) => window.setTimeout(callback, delay),
  clearTimeout: (handle) => window.clearTimeout(handle as number),
};

function isComplete(value: AccountTaskProgressDTO | undefined): boolean {
  return value !== undefined && value.completed >= value.total;
}

function sameProgress(left: AccountTaskProgressDTO | undefined, right: AccountTaskProgressDTO): boolean {
  return left?.completed === right.completed && left.total === right.total && left.phase === right.phase;
}

function monotonicProgress(current: AccountTaskProgressDTO | undefined, next: AccountTaskProgressDTO): AccountTaskProgressDTO {
  if (!current) return next;
  return {
    ...next,
    completed: Math.max(current.completed, next.completed),
    total: Math.max(current.total, next.total),
  };
}

// Progress phases are produced concurrently by the backend sync pipeline. This
// controller buffers future phases, advances the visible phase only forwards,
// and applies one global render throttle across the whole task.
export function createAccountTaskProgressController(options: AccountTaskProgressControllerOptions): AccountTaskProgressController {
  const onProgress = options.onProgress;
  const phases = options.phases ?? [];
  const intervalMs = options.intervalMs ?? 100;
  const scheduler = options.scheduler ?? browserScheduler;
  const phaseIndexes = new Map(phases.map((phase, index) => [phase, index]));
  const latestByPhase = new Map<AccountTaskProgressPhase, AccountTaskProgressDTO>();
  const latestUnordered = new Map<AccountTaskProgressPhase | undefined, AccountTaskProgressDTO>();
  let activePhaseIndex = 0;
  let pending: AccountTaskProgressDTO | undefined;
  let lastEmitted: AccountTaskProgressDTO | undefined;
  let lastEmittedAt: number | undefined;
  let timer: unknown;
  let disposed = false;

  const clearTimer = () => {
    if (timer === undefined) return;
    scheduler.clearTimeout(timer);
    timer = undefined;
  };

  const emitPending = () => {
    if (!pending || !onProgress || disposed) return;
    const value = pending;
    pending = undefined;
    if (sameProgress(lastEmitted, value)) return;
    lastEmitted = value;
    lastEmittedAt = scheduler.now();
    onProgress(value);
  };

  const schedule = (value: AccountTaskProgressDTO) => {
    if (!onProgress || disposed || sameProgress(pending ?? lastEmitted, value)) return;
    pending = value;
    if (lastEmittedAt === undefined) {
      clearTimer();
      emitPending();
      return;
    }
    const delay = Math.max(0, intervalMs - (scheduler.now() - lastEmittedAt));
    if (delay === 0) {
      clearTimer();
      emitPending();
    } else if (timer === undefined) {
      timer = scheduler.setTimeout(() => {
        timer = undefined;
        emitPending();
      }, delay);
    }
  };

  const reportOrdered = (value: AccountTaskProgressDTO) => {
    const phase = value.phase ?? phases[0];
    const phaseIndex = phase === undefined ? undefined : phaseIndexes.get(phase);
    if (phase === undefined || phaseIndex === undefined || phaseIndex < activePhaseIndex) return;

    const normalized = monotonicProgress(latestByPhase.get(phase), { ...value, phase });
    latestByPhase.set(phase, normalized);
    const previousActivePhaseIndex = activePhaseIndex;

    // A future phase may arrive before the current phase finishes. Keep it
    // buffered until completion, then advance without ever regressing.
    while (activePhaseIndex < phases.length - 1) {
      const active = latestByPhase.get(phases[activePhaseIndex]);
      const next = latestByPhase.get(phases[activePhaseIndex + 1]);
      if (!isComplete(active) || !next) break;
      activePhaseIndex += 1;
    }

    const active = latestByPhase.get(phases[activePhaseIndex]);
    if (!active) return;
    if (activePhaseIndex !== previousActivePhaseIndex && onProgress) {
      // A phase transition is a low-frequency semantic event. Publish it
      // immediately while ordinary counters remain globally throttled.
      pending = active;
      clearTimer();
      emitPending();
      return;
    }
    schedule(active);
  };

  return {
    report(value) {
      if (disposed) return;
      if (phases.length > 0) {
        reportOrdered(value);
        return;
      }
      const normalized = monotonicProgress(latestUnordered.get(value.phase), value);
      latestUnordered.set(value.phase, normalized);
      schedule(normalized);
    },
    flush() {
      if (disposed) return;
      clearTimer();
      emitPending();
    },
    dispose() {
      clearTimer();
      pending = undefined;
      disposed = true;
    },
  };
}
