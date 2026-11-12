package outbox

import (
	"context"
	"time"
)

func (d *Dispatcher) SetNowForTest(now func() time.Time) { d.now = now }

func (d *Dispatcher) DrainForTest(ctx context.Context) { d.Flush(ctx, d.now()) }
