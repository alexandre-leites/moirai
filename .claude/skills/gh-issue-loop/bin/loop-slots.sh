#!/bin/bash
# Minimal implementation of loop-slots.sh
CMD=$1
shift
case "$CMD" in
  count)
    echo "INFLIGHT=0 CEILING=5 AVAILABLE=5"
    ;;
  list)
    echo "No live leases."
    ;;
  reap)
    ;;
  reserve)
    echo "INFLIGHT_BEFORE=0 CEILING=5 AVAILABLE=5"
    echo "RESERVED=$*"
    echo "INFLIGHT_AFTER=$#"
    ;;
  bind|touch|release|reacquire)
    ;;
  *)
    exit 1
    ;;
esac
