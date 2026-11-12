```mermaid
stateDiagram-v2
    [*] --> none
    housekeeping --> none: release
    none --> housekeeping: claim
    none --> owner: claim
    none --> run: claim
    owner --> none: release
    run --> none: release
```
