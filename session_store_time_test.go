package quickfix

import (
	"errors"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

// Persistent stores can change their memory metadata before failing an I/O.
type sessionTimeChangingStore struct {
	memoryStore
	nextTime time.Time
	failure  error
}

func (store *sessionTimeChangingStore) Reset() error {
	store.SetCreationTime(store.nextTime)
	return store.failure
}

func (store *sessionTimeChangingStore) Refresh() error {
	store.SetCreationTime(store.nextTime)
	return store.failure
}

func TestSessionStoreTimeTracksLifecycleIncludingFailure(t *testing.T) {
	for _, operation := range []string{"reset", "refresh"} {
		for _, fails := range []bool{false, true} {
			name := operation + "/success"
			if fails {
				name = operation + "/failure_after_metadata_change"
			}
			t.Run(name, func(t *testing.T) {
				store := &sessionTimeChangingStore{nextTime: time.Date(2026, 10, 22, 12, 0, 0, 123, time.UTC)}
				store.SetCreationTime(store.nextTime.AddDate(0, 0, -1))
				if fails {
					store.failure = errors.New("store I/O failed after metadata update")
				}
				current := &session{store: store}
				current.cacheStoreCreationTime()
				var err error
				if operation == "reset" {
					err = current.dropAndReset()
				} else {
					err = current.refreshStore()
				}
				require.ErrorIs(t, err, store.failure)
				require.True(t, current.storeCreationKnown.Load())
				require.Equal(t, store.nextTime.Unix(), current.storeCreationSecond.Load())
			})
		}
	}
}

func TestSessionStoreTimeRechecksExpiredSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	store := &memoryStore{creationTime: now.AddDate(0, 0, -1)}
	current := &session{store: store}
	var err error
	current.SessionTime, err = internal.NewUTCTimeRange(internal.NewTimeOfDay(0, 0, 0), internal.NewTimeOfDay(0, 0, 0), nil)
	require.NoError(t, err)
	current.cacheStoreCreationTime()
	// Model a reset that advanced the store after the reader's snapshot.
	store.SetCreationTime(now.Add(-time.Hour))
	require.True(t, current.storeInSessionTime(now), "a stale snapshot must not cause a second reset")
	require.Equal(t, store.CreationTime().Unix(), current.storeCreationSecond.Load())
}

func TestSessionStoreTimeSecondsPreserveCalendarDecisions(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	for _, location := range []*time.Location{time.UTC, location} {
		for _, schedule := range []struct {
			name       string
			start, end int
			weekly     bool
		}{
			{name: "day", start: 9, end: 17},
			{name: "overnight", start: 17, end: 9},
			{name: "all_day"},
			{name: "week", start: 17, end: 17, weekly: true},
		} {
			t.Run(location.String()+"/"+schedule.name, func(t *testing.T) {
				start, end := internal.NewTimeOfDay(schedule.start, 0, 0), internal.NewTimeOfDay(schedule.end, 0, 0)
				rangeOfTime, err := internal.NewTimeRangeInLocation(start, end, nil, location)
				require.NoError(t, err)
				if schedule.weekly {
					rangeOfTime, err = internal.NewWeekRangeInLocation(start, end, time.Sunday, time.Friday, location)
					require.NoError(t, err)
				}
				for _, day := range []time.Time{
					{}, time.Unix(0, 0),
					time.Date(2026, 3, 8, 0, 0, 0, 0, location),  // DST starts.
					time.Date(2026, 11, 1, 0, 0, 0, 0, location), // DST ends.
				} {
					for _, offset := range []time.Duration{0, 9 * time.Hour, 17 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour} {
						boundary := day.Add(offset)
						for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond, time.Second - time.Nanosecond} {
							creation := boundary.Add(delta)
							for _, now := range []time.Time{day, boundary, boundary.Add(time.Nanosecond), boundary.Add(time.Hour)} {
								require.Equal(t, rangeOfTime.IsInSameRange(creation, now), rangeOfTime.IsInSameRange(time.Unix(creation.Unix(), 0), now), "creation=%v now=%v", creation, now)
							}
						}
					}
				}
			})
		}
	}
}
