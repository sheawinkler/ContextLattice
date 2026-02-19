# Performance Baseline

We target **100 writes/sec** on `/memory/write` as a baseline. The orchestrator now queues memory bank writes plus fanout work (Qdrant/Langfuse/MindsDB/Letta) to keep write latency low.

## Quick Load Test

```bash
python3 scripts/load_test_memory_write.py --rate 100 --seconds 10 --threads 20 --payload-bytes 256
```

Recommended knobs if you see lag:
- `MEMORY_WRITE_ASYNC=true` (default) to avoid blocking on disk writes
- Increase `MEMORY_BANK_WORKERS` for more concurrent memory writes
- Increase `MEMORY_BANK_QUEUE_MAX` if write bursts queue up
- Increase `MEMORY_WRITE_WORKERS` (default 4)
- Increase `MEMORY_WRITE_QUEUE_MAX` if fanout bursts
- Keep `MEMORY_WRITE_DEDUP_ENABLED=true` to suppress duplicate posts
- Tune `MEMORY_WRITE_DEDUP_WINDOW_SECS` (default `120`) for your retry behavior
- Keep `FANOUT_COALESCE_ENABLED=true` to collapse repeated writes for hot files
- Tune `FANOUT_COALESCE_WINDOW_SECS` (default `6`) and `FANOUT_COALESCE_TARGETS`
- Keep `LETTA_ADMISSION_ENABLED=true` to prevent Letta backlog from cascading
- Ensure embedding provider is fast and local for testing

## Docker Log Pressure

Noisy services can consume Docker VM disk via `json-file` logs even when image size is stable.

Use:
- `SUPERGATEWAY_LOG_LEVEL=none` for `memorymcp-http`
- `DOCKER_LOG_MAX_SIZE=25m`
- `DOCKER_LOG_MAX_FILE=4`

## Notes
- The test writes into the memory bank, so you can cleanup the `perf_test` project if needed.
- Fanout queue drops are logged as `memory.write.fanout_drop` in orchestrator logs.
