#!/usr/bin/env python3
# Copyright The Volcano Authors.
#
# E2E helper: bridge llm-d-inference-sim ZMQ (kv@IP@model) to Kthena Runtime (kv-events).

import logging
import os
import signal
import sys

import msgpack
import zmq

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("zmq-bridge")

SIM_SUB_BIND = os.getenv("SIM_SUB_BIND", "tcp://127.0.0.1:5556")
RUNTIME_PUB_BIND = os.getenv("RUNTIME_PUB_BIND", "tcp://127.0.0.1:5557")
SIM_TOPIC_PREFIX = os.getenv("SIM_TOPIC_PREFIX", "kv@")
RUNTIME_TOPIC = os.getenv("RUNTIME_TOPIC", "kv-events")

running = True


def _shutdown(signum, _frame) -> None:
    global running
    logger.info("received signal %s, shutting down", signum)
    running = False


def main() -> int:
    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)

    ctx = zmq.Context()
    sub = ctx.socket(zmq.SUB)
    sub.bind(SIM_SUB_BIND)
    sub.setsockopt_string(zmq.SUBSCRIBE, SIM_TOPIC_PREFIX)
    sub.setsockopt(zmq.RCVTIMEO, 1000)

    pub = ctx.socket(zmq.PUB)
    pub.bind(RUNTIME_PUB_BIND)

    logger.info(
        "zmq-bridge listening sim=%s (prefix=%s) runtime=%s (topic=%s)",
        SIM_SUB_BIND,
        SIM_TOPIC_PREFIX,
        RUNTIME_PUB_BIND,
        RUNTIME_TOPIC,
    )

    forwarded = 0
    while running:
        try:
            parts = sub.recv_multipart()
        except zmq.Again:
            continue
        except zmq.ZMQError as exc:
            if running:
                logger.error("recv failed: %s", exc)
            break

        if len(parts) < 3:
            logger.warning("ignored message with %d parts", len(parts))
            continue

        topic = parts[0].decode("utf-8", errors="replace")
        if not topic.startswith(SIM_TOPIC_PREFIX):
            continue

        payload = parts[2]

        # Best-effort introspection: help debug whether sim payload contains token ids.
        if forwarded == 0:
            try:
                obj = msgpack.unpackb(payload, raw=False)
                token_ids_present = False
                if isinstance(obj, dict):
                    events = obj.get("events")
                    if isinstance(events, list) and events:
                        first = events[0]
                        if isinstance(first, dict) and "token_ids" in first:
                            token_ids_present = True
                logger.info("first payload decoded type=%s token_ids_present=%s", type(obj).__name__, token_ids_present)
            except Exception as exc:
                logger.info("first payload msgpack decode failed: %s", exc)

        pub.send_multipart([RUNTIME_TOPIC.encode("utf-8"), b"", payload])
        forwarded += 1
        if forwarded == 1 or forwarded % 10 == 0:
            logger.info("forwarded %d batches (latest topic=%s, %d bytes)", forwarded, topic, len(payload))

    sub.close()
    pub.close()
    ctx.term()
    return 0


if __name__ == "__main__":
    sys.exit(main())

