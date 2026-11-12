```mermaid
stateDiagram-v2
    [*] --> running: created
    done --> [*]
    failed_at_step --> [*]
    failed_at_step --> done: owner_continued
    failed_at_step --> running: owner_continued
    running --> done: step_finished
    running --> failed_at_step: interrupted
    running --> failed_at_step: step_finished
    running --> running: step_finished
    running --> running: step_started
```
