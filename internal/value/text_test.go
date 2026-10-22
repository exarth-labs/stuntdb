package value

import (
	"testing"
	"time"
)

func TestISORoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 19, 13, 45, 0, 123456000, time.UTC)
	tsZone := time.Date(2026, 10, 19, 13, 45, 0, 0, time.FixedZone("", 5*3600))
	timeZone, err := TimeOfDayWithZone(13*time.Hour+45*time.Minute, -(3*3600 + 30*60))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		v    Value
		sep  byte
		want string
	}{
		{"date", mustDate(t, 2026, 10, 19), ' ', "2026-10-19"},
		{"date before year 1", mustDate(t, -43, 3, 15), ' ', "-0043-03-15"},
		{"date after year 9999", mustDate(t, 12026, 1, 1), ' ', "+12026-01-01"},
		{"time", mustTime(t, 13*time.Hour+45*time.Minute+250*time.Millisecond), ' ', "13:45:00.25"},
		{"time 24:00", mustTime(t, 24*time.Hour), ' ', "24:00:00"},
		{"time with zone", timeZone, ' ', "13:45:00-03:30"},
		{"timestamp with a space", Timestamp(ts), ' ', "2026-10-19 13:45:00.123456"},
		{"timestamp with a T", Timestamp(ts), 'T', "2026-10-19T13:45:00.123456"},
		{"instant", TimestampTZ(ts), ' ', "2026-10-19 13:45:00.123456Z"},
		{"instant with offset", TimestampTZWithZone(tsZone), ' ', "2026-10-19 13:45:00+05:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := AppendISO(nil, tt.v, tt.sep)
			if !ok || string(got) != tt.want {
				t.Fatalf("AppendISO = %q, %v; want %q", got, ok, tt.want)
			}
			back, err := ParseISO(tt.v.Kind(), tt.want, tt.sep)
			if err != nil {
				t.Fatalf("ParseISO: %v", err)
			}
			if !back.Equal(tt.v) {
				t.Error("ParseISO did not return the original value")
			}
		})
	}
}

func TestAppendISORejects(t *testing.T) {
	for _, v := range []Value{
		Null(), Int(1), String("2026-10-19"),
		DateInfinity(false), TimestampInfinity(true, true), MySQLZeroDate("0000-00-00"),
	} {
		if got, ok := AppendISO([]byte("x"), v, 'T'); ok || string(got) != "x" {
			t.Errorf("AppendISO(%s value) = %q, %v; want no change and false", v.Kind(), got, ok)
		}
	}
}

func TestParseISORejects(t *testing.T) {
	tests := []struct {
		k   Kind
		s   string
		sep byte
	}{
		{KindDate, "2026-02-30", ' '},
		{KindDate, "2026-10-19 00:00:00", ' '},
		{KindDate, "26-10-19", ' '},
		{KindTime, "25:00:00", ' '},
		{KindTime, "13:45", ' '},
		{KindTimestamp, "2026-10-19T13:45:00", ' '},
		{KindTimestamp, "2026-10-19 13:45:00", 'T'},
		{KindTimestamp, "2026-10-19 24:00:00", ' '},
		{KindTimestamp, "2026-10-19 13:45:00+5", ' '},
		{KindString, "2026-10-19", ' '},
	}
	for _, tt := range tests {
		if _, err := ParseISO(tt.k, tt.s, tt.sep); err == nil {
			t.Errorf("ParseISO(%s, %q, %q) succeeded", tt.k, tt.s, tt.sep)
		}
	}
}

func TestFormatUUID(t *testing.T) {
	const s = "123e4567-e89b-12d3-a456-426614174000"
	v, err := ParseUUID(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatUUID(v.UUID()); got != s {
		t.Errorf("FormatUUID = %q, want %q", got, s)
	}
}
