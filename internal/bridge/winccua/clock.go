package winccua

import "time"

func defaultNowNanos() int64 { return time.Now().UnixNano() }
