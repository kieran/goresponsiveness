/*
 * This file is part of Go Responsiveness.
 *
 * Go Responsiveness is free software: you can redistribute it and/or modify it under
 * the terms of the GNU General Public License as published by the Free Software Foundation,
 * either version 2 of the License, or (at your option) any later version.
 * Go Responsiveness is distributed in the hope that it will be useful, but WITHOUT ANY
 * WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
 * PARTICULAR PURPOSE. See the GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License along
 * with Go Responsiveness. If not, see <https://www.gnu.org/licenses/>.
 */

package utilities

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/constraints"
)

var (
	// GitVersion is the Git revision hash
	GitVersion = "dev"
)

func Iota(low int, high int) (made []int) {

	made = make([]int, high-low)
	for counter := low; counter < high; counter++ {
		made[counter-low] = counter
	}
	return
}

func IsInterfaceNil(ifc interface{}) bool {
	return ifc == nil ||
		(reflect.ValueOf(ifc).Kind() == reflect.Ptr && reflect.ValueOf(ifc).IsNil())
}

func SignedPercentDifference[T constraints.Float | constraints.Integer](
	current T,
	previous T,
) (difference float64) {
	fCurrent := float64(current)
	fPrevious := float64(previous)
	return ((fCurrent - fPrevious) / fPrevious) * 100.0
}

func AbsPercentDifference(
	current float64,
	previous float64,
) (difference float64) {
	return (math.Abs(current-previous) / (float64(current+previous) / 2.0)) * float64(
		100,
	)
}

func Conditional(condition bool, t string, f string) string {
	if condition {
		return t
	}
	return f
}

func ToMbps(bytes float64) float64 {
	return ToMBps(bytes) * float64(8)
}

func ToMBps(bytes float64) float64 {
	return float64(bytes) / float64(1024*1024)
}

type MeasurementResult struct {
	Delay            time.Duration
	MeasurementCount uint16
	Err              error
}

func SeekForAppend(file *os.File) (err error) {
	_, err = file.Seek(0, 2)
	return
}

var GenerateUniqueId func() uint64 = func() func() uint64 {
	var nextConnectionId uint64 = 0
	return func() uint64 {
		return atomic.AddUint64(&nextConnectionId, 1)
	}
}()

type Optional[S any] struct {
	value S
	some  bool
}

func Some[S any](value S) Optional[S] {
	return Optional[S]{value: value, some: true}
}

func None[S any]() Optional[S] {
	return Optional[S]{some: false}
}

func IsNone[S any](optional Optional[S]) bool {
	return !optional.some
}

func IsSome[S any](optional Optional[S]) bool {
	return optional.some
}

func GetSome[S any](optional Optional[S]) S {
	if !optional.some {
		panic("Attempting to access Some of a None.")
	}
	return optional.value
}

func (optional Optional[S]) String() string {
	if IsSome(optional) {
		return fmt.Sprintf("Some: %v", optional.some)
	} else {
		return "None"
	}
}

func RandBetween(max int) int {
	return rand.New(rand.NewSource(int64(time.Now().Nanosecond()))).Int() % max
}

func Max(x, y uint64) uint64 {
	if x > y {
		return x
	}
	return y
}

func ChannelToSlice[S any](channel <-chan S) (slice []S) {
	slice = make([]S, 0)
	for element := range channel {
		slice = append(slice, element)
	}
	return
}

func Fmap[S any, F any](elements []S, mapper func(S) F) []F {
	result := make([]F, 0)
	for _, s := range elements {
		result = append(result, mapper(s))
	}
	return result
}

func CalculatePercentile[S float32 | int32 | float64 | int64](elements []S, percentile int) S {
	sort.Slice(elements, func(a, b int) bool { return elements[a] < elements[b] })
	elementsCount := len(elements)
	percentileIdx := elementsCount * (percentile / 100)
	return elements[percentileIdx]
}

func OrTimeout(f func(), timeout time.Duration) {
	completeChannel := func() chan interface{} {
		completed := make(chan interface{})
		go func() {
			// This idea taken from https://stackoverflow.com/a/32843750.
			// Closing the channel will let the read of it proceed immediately.
			// Making that operation a defer ensures that it will happen even if
			// the function f panics during its execution.
			defer close(completed)
			f()
		}()
		return completed
	}()
	select {
	case <-completeChannel:
		break
	case <-time.After(timeout):
		break
	}
}

func FilenameAppend(filename, appendage string) string {
	pieces := strings.SplitN(filename, ".", 2)
	result := pieces[0] + appendage
	if len(pieces) > 1 {
		result = result + "." + strings.Join(pieces[1:], ".")
	}
	return result
}

func ApproximatelyEqual[T float32 | float64](truth T, maybe T, fudge T) bool {
	bTruth := float64(truth)
	bMaybe := float64(maybe)
	bFudge := float64(fudge)
	diff := math.Abs((bTruth - bMaybe))
	return diff < bFudge
}

func UserAgent() string {
	return fmt.Sprintf("goresponsiveness/%s", GitVersion)
}

func WaitWithContext(ctxt context.Context, condition *func() bool, mu *sync.Mutex, c *sync.Cond) bool {
	mu.Lock()
	for !(*condition)() && ctxt.Err() == nil {
		c.Wait()
	}
	return ctxt.Err() == nil
}

func ContextSignaler(ctxt context.Context, st time.Duration, condition *func() bool, c *sync.Cond) {
	for !(*condition)() && ctxt.Err() == nil {
		time.Sleep(st)
	}
	if ctxt.Err() != nil {
		c.Broadcast()
		return
	}
}
