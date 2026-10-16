package rustle

import (
	"sync"
	"time"
)

// RecordFile is a source's record in its cache folder.
const RecordFile = "record.json"

// remoteTTL: signed YouTube media URLs expire after about six hours.
const remoteTTL = 5 * time.Hour

var records = struct {
	sync.Mutex
	memory  map[string]*Record
	unsaved map[string]bool
	writes  sync.WaitGroup
}{memory: map[string]*Record{}, unsaved: map[string]bool{}}

// Fresh reports whether a record can still be used: local ones always, remote ones until
// their URLs may have expired.
func (r *Record) Fresh() bool {
	return r != nil && (r.Kind == "local" || time.Since(time.UnixMilli(r.ResolvedAt)) < remoteTTL)
}

// SaveRecord keeps what a call learned (probe results, YouTube state) for the next call
// and process. The disk copy is for the next process, so it is written once the call has
// moved on.
func SaveRecord(record *Record, cache *Cache) {
	records.Lock()
	defer records.Unlock()
	records.memory[record.Key] = record
	if records.unsaved[record.Key] {
		return
	}
	records.unsaved[record.Key] = true
	records.writes.Add(1)
	go func() {
		defer records.writes.Done()
		records.Lock()
		delete(records.unsaved, record.Key)
		latest := records.memory[record.Key]
		records.Unlock()
		// A full or read-only cache loses the disk copy, not the call.
		_ = cache.WriteJSON(record.Key, RecordFile, latest)
	}()
}

// KnownRecord is the record in memory, else on disk, if one is still fresh.
func KnownRecord(key string, cache *Cache) *Record {
	records.Lock()
	known := records.memory[key]
	records.Unlock()
	if known == nil {
		var stored Record
		if cache.ReadJSON(key, RecordFile, &stored) && stored.Key != "" {
			known = &stored
		}
	}
	if !known.Fresh() {
		return nil
	}
	records.Lock()
	records.memory[key] = known
	records.Unlock()
	return known
}

// FlushRecords waits for records still being written to disk.
func FlushRecords() { records.writes.Wait() }
