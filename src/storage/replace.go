package storage

// CompactTempSuffix names the temporary file compaction writes next to the
// data file. It is never authoritative while the data file exists.
const CompactTempSuffix = ".compact.tmp"
