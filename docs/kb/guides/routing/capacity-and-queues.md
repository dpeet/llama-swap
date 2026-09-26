---
title: Routing capacity and request queues
summary: Configure concurrencyLimit, globalConcurrencyLimit and memory admission, and understand queued work while a model is loading or busy.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit, memory, memory-admission, unified-memory, oom]
config_keys: [routing, models.*.concurrencyLimit, globalConcurrencyLimit, memoryPool, memoryReserve, models.*.memoryCeiling]
updated: 2026-09-25
---

# Routing capacity and request queues

There is no `models.*.maxConcurrentRequests` setting. The real per-model key
is `models.*.concurrencyLimit`, which limits active parallel requests.

The router serializes model swaps and queues requests until the selected model
is ready. Avoid using many simultaneous client retries as a capacity control:
they create more queued work. Choose a routing policy that fits the models that
may coexist, then set client timeouts high enough for the queue and load time.

Inspect the Activity view and logs when latency grows. A queue that never drains
usually means the command, proxy, or health check is wrong; see
`guides/model-runtime/troubleshooting-model-wont-load`.

## Global concurrency limit

`globalConcurrencyLimit` is a top-level setting, separate from
`models.*.concurrencyLimit`. It caps the number of inference requests served
at once across every model combined, using a single shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default is `0`, meaning no limit — the semaphore is not even added to the
request chain, so a default config pays no cost for the feature. Once the
limit is reached, further requests are rejected immediately with an HTTP 429
response rather than queued; there is no wait for a slot to free up. Clients
should treat a 429 as a signal to back off and retry, the same way they handle
a per-model concurrency limit rejection.

Use this to protect shared hardware (CPU, disk, network) from being
overwhelmed by traffic spread across many different models, which a per-model
`concurrencyLimit` cannot do since it only counts requests to one model at a
time.

## Memory admission

On a box where models share one memory pool (e.g. unified GPU/CPU memory), you
can have llama-swap refuse loads that would not fit instead of letting the
kernel OOM-kill something. All values are bytes; `memoryPool: 0` (the default)
turns the feature off.

```yaml
memoryPool: 129922760704      # 121 GiB usable
memoryReserve: 10737418240    # keep 10 GiB free -> budget is pool - reserve
models:
  big-llm:
    memoryCeiling: 107374182400  # measured steady-state footprint
  asr:
    memoryCeiling: 8589934592
```

A new load is admitted only when its `memoryCeiling` plus the ceilings of every
model that stays resident (running models minus the ones the swap will evict)
fits the budget. If it does not:

- It is refused at once with HTTP 503 (`code: memory_admission`) when its own
  ceiling exceeds the budget, when it has no `memoryCeiling`, or when the
  models holding the budget are ones the router will not evict for it and
  nothing in progress (a swap, a model stopping, a force-killed model still
  being watched, see below) could free memory. The message
  names the models holding the budget. Unload one of them, or change groups so
  the load evicts it.
- It queues only while such work is in progress, and is served or refused with
  the same 503 once that settles. A streaming client already receiving the
  loading stream gets the refusal as an SSE error event followed by
  `data: [DONE]`.

If stopping a model had to force-kill it (its `unloadTimeout` ran out), its
container may still hold memory although llama-swap shows it stopped. With
`memoryPool` set, llama-swap keeps counting that model's `memoryCeiling` and
probes its `proxy` + `checkEndpoint` every 5 seconds; the ceiling is released
only once nothing answers there (a 5xx still counts as up), or when the model
is started again. Loads that need that memory queue meanwhile rather than fail.
The log shows `was force-killed; counting its ... memoryCeiling` and later
`leaked model ... cleared`. A container that never goes away blocks those loads
until you stop it by hand.

Already-running models and adopt attaches are never refused. Measure ceilings
under real load: an under-sized ceiling lets two models in that do not fit.
