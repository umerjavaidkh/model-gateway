package memkv

import "strconv"

// Counters are stored as decimal text rather than binary, so a value written
// here and a value written by Redis INCR are the same bytes. That keeps the
// two stores interchangeable, which is the point of the interface.

func parseInt(b []byte) (int64, error) { return strconv.ParseInt(string(b), 10, 64) }

func formatInt(v int64) []byte { return []byte(strconv.FormatInt(v, 10)) }
