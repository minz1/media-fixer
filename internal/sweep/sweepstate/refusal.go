package sweepstate

import "time"

type RefusalCode string

const (
	TorrentBlocked RefusalCode = "torrent_blocked"
	ProviderError  RefusalCode = "provider_error"
	OtherRefusal   RefusalCode = "other"
)

func RefusalCodes() []RefusalCode {
	return []RefusalCode{TorrentBlocked, ProviderError, OtherRefusal}
}

func (c RefusalCode) Memory() time.Duration {
	switch c {
	case TorrentBlocked:
		return 90 * 24 * time.Hour
	case OtherRefusal:
		return 7 * 24 * time.Hour
	case ProviderError:
		return 0
	}
	return 0
}

func (c RefusalCode) Blocks() bool {
	switch c {
	case TorrentBlocked, OtherRefusal:
		return true
	case ProviderError:
		return false
	}
	return false
}

func MaxRefusalMemory() time.Duration {
	var longest time.Duration
	for _, c := range RefusalCodes() {
		longest = max(longest, c.Memory())
	}
	return longest
}
