module github.com/vibeslop/rustle-mcp

// Go 1.27.3 is the first to reject certificates signed with the eight eMudhra CA keys
// leaked in October 2026. 1.26.10 has the fix too, but 1.27.0 to 1.27.2 would satisfy it.
go 1.27.3

// Published from earlier trees, which the Go module proxy keeps.
retract [v1.0.0, v1.0.2]
