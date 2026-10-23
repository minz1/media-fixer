package sweep

func (s *Scheduler) Wait() { s.wg.Wait() }
