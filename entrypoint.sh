#!/bin/bash
set -e

WORKERS=${WORKERS:-4}

echo "Starting Grok2API with $WORKERS workers..."

exec python -m uvicorn main:app --host 0.0.0.0 --port 8000 --workers $WORKERS
