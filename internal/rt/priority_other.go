//go:build !windows && !(darwin && (amd64 || arm64)) && (!linux || (!amd64 && !arm64))

package rt

import "time"

func raisePriority(_ time.Duration) Grant { return Grant{Kind: "normal"} }
