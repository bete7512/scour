// Package scour walks directory trees in parallel.
//
// It is meant for very large trees on parallel and network filesystems
// (Lustre, GPFS, BeeGFS, NFS), where wall time is decided by how many
// metadata requests are in flight and how many of them are stat calls.
// scour reads many directories at once, classifies entries from the
// directory listing without calling stat, and can cap the rate of
// metadata operations so a scan on a shared cluster leaves room for
// everyone else.
//
// [Walk] calls a function for every entry from worker goroutines,
// concurrently. [Scan] delivers entries on a channel to a single
// consumer. Entries are not sorted. A directory is delivered before its
// contents and the root is delivered first; nothing else about the order
// is promised.
package scour
