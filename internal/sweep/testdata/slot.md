```mermaid
stateDiagram-v2
    [*] --> idle
    dry --> dry: cancel_live
    dry --> idle: done
    dry --> stopped: stop
    idle --> dry: start
    idle --> idle: cancel_live
    idle --> idle: reconcile
    idle --> live: start
    idle --> stopped: stop
    live --> idle: done
    live --> live: cancel_live
    live --> stopped: stop
    stopped --> [*]
    stopped --> stopped: done
```
