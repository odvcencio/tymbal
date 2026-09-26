//go:build !windows && (!linux || (!amd64 && !arm64))

package rt

import "time"

func raisePriority(_ time.Duration) Grant { return Grant{Kind: "normal"} }
