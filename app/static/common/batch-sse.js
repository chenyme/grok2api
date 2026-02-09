(function (global) {
  function openBatchStream(taskId, apiKey, options = {}) {
    if (!taskId) return null;

    const handlers = options || {};
    const streamToken = String(handlers.streamToken || '').trim();
    if (!streamToken) {
      if (handlers.onError) {
        handlers.onError(new Error('Missing stream token'));
      }
      return null;
    }

    const url = `/api/v1/admin/batch/${taskId}/stream?stream_token=${encodeURIComponent(streamToken)}`;
    const es = new EventSource(url);

    es.onmessage = (e) => {
      if (!e.data) return;
      let msg;
      try {
        msg = JSON.parse(e.data);
      } catch {
        return;
      }
      if (handlers.onMessage) handlers.onMessage(msg);
    };

    es.onerror = () => {
      if (handlers.onError) handlers.onError();
    };

    return es;
  }

  function closeBatchStream(es) {
    if (es) es.close();
  }

  async function cancelBatchTask(taskId, apiKey) {
    if (!taskId) return;
    try {
      const rawKey = String(apiKey || '').trim();
      const auth = rawKey.startsWith('Bearer ') ? rawKey : (rawKey ? `Bearer ${rawKey}` : '');
      await fetch(`/api/v1/admin/batch/${taskId}/cancel`, {
        method: 'POST',
        headers: auth ? { Authorization: auth } : undefined
      });
    } catch {
      // ignore
    }
  }

  global.BatchSSE = {
    open: openBatchStream,
    close: closeBatchStream,
    cancel: cancelBatchTask
  };
})(window);
