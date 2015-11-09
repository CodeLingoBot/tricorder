package tricorder

import (
	"fmt"
	"github.com/Symantec/tricorder/go/tricorder/messages"
	"github.com/Symantec/tricorder/go/tricorder/types"
	"github.com/Symantec/tricorder/go/tricorder/units"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	panicBadFunctionReturnTypes = "Functions must return either T or (T, error) where T is a primitive numeric type or a string."
	panicInvalidMetric          = "Invalid metric type."
	panicIncompatibleTypes      = "Wrong AsXXX function called on value."
)

var (
	root = newDirectory()
)

// bucketPiece represents a single range in a distribution
type bucketPiece struct {
	// Start value of range inclusive
	Start float64
	// End value of range exclusive
	End float64
	// If true range is of the form < End.
	First bool
	// If true range is of the form >= Start.
	Last bool
}

func newExponentialBucketerStream(
	count int, start, scale float64) (int, <-chan float64) {
	if count < 2 || start <= 0.0 || scale <= 1.0 {
		panic("count >= 2 && start > 0.0 && scale > 1")
	}
	stream := make(chan float64)
	go func() {
		current := start
		for i := 0; i < count-1; i++ {
			stream <- current
			current *= scale
		}
		close(stream)
	}()
	return count - 1, stream
}

func newLinearBucketerStream(
	count int, start, increment float64) (int, <-chan float64) {
	if count < 2 || increment <= 0.0 {
		panic("count >= 2 && increment > 0")
	}
	stream := make(chan float64)
	go func() {
		current := start
		for i := 0; i < count-1; i++ {
			stream <- current
			current += increment
		}
		close(stream)
	}()
	return count - 1, stream
}

func newArbitraryBucketerStream(endpoints []float64) (int, <-chan float64) {
	if len(endpoints) == 0 {
		panic("endpoints must have at least one element.")
	}
	stream := make(chan float64)
	go func() {
		for _, endpoint := range endpoints {
			stream <- endpoint
		}
		close(stream)
	}()
	return len(endpoints), stream
}

func newBucketerFromStream(
	streamSize int, stream <-chan float64) *Bucketer {
	if streamSize < 1 {
		panic("streamSize must be at least 1")
	}
	pieces := make([]*bucketPiece, streamSize+1)
	lower := <-stream
	pieces[0] = &bucketPiece{First: true, End: lower}
	idx := 1
	for upper := range stream {
		pieces[idx] = &bucketPiece{
			Start: lower, End: upper}
		lower = upper
		idx++
	}
	pieces[idx] = &bucketPiece{
		Last: true, Start: lower}
	return &Bucketer{pieces: pieces}
}

// breakdownPiece represents a single range and count pair in a
// distribution breakdown
type breakdownPiece struct {
	*bucketPiece
	Count uint64
}

// breakdown represents a distribution breakdown.
type breakdown []breakdownPiece

// snapshot represents a snapshot of a distribution
type snapshot struct {
	Min     float64
	Max     float64
	Average float64

	// TODO: Have to discuss how to implement this
	Median    float64
	Count     uint64
	Breakdown breakdown
}

// distribution represents a distribution of values same as Distribution
type distribution struct {
	// Protects all fields except pieces whose contents never changes
	lock   sync.RWMutex
	pieces []*bucketPiece
	counts []uint64
	total  float64
	min    float64
	max    float64
	count  uint64
}

func newDistribution(bucketer *Bucketer) *distribution {
	return &distribution{
		pieces: bucketer.pieces,
		counts: make([]uint64, len(bucketer.pieces)),
	}
}

// Add adds a value to this distribution
func (d *distribution) Add(value float64) {
	idx := findDistributionIndex(d.pieces, value)
	d.lock.Lock()
	defer d.lock.Unlock()
	d.counts[idx]++
	d.total += value
	if d.count == 0 {
		d.min = value
		d.max = value
	} else if value < d.min {
		d.min = value
	} else if value > d.max {
		d.max = value
	}
	d.count++
}

func findDistributionIndex(pieces []*bucketPiece, value float64) int {
	return sort.Search(len(pieces)-1, func(i int) bool {
		return value < pieces[i].End
	})
}

func valueIndexToPiece(counts []uint64, valueIdx float64) (
	pieceIdx int, frac float64) {
	pieceIdx = 0
	startValueIdxInPiece := -0.5
	for valueIdx-startValueIdxInPiece >= float64(counts[pieceIdx]) {
		startValueIdxInPiece += float64(counts[pieceIdx])
		pieceIdx++
	}
	return pieceIdx, (valueIdx - startValueIdxInPiece) / float64(counts[pieceIdx])

}

func interpolate(min float64, max float64, frac float64) float64 {
	return (1.0-frac)*min + frac*max
}

func (d *distribution) calculateMedian() float64 {
	if d.count == 1 {
		return d.min
	}
	medianIndex := float64(d.count-1) / 2.0
	pieceIdx, frac := valueIndexToPiece(d.counts, medianIndex)
	pieceLen := len(d.pieces)
	if pieceIdx == 0 {
		return interpolate(
			d.min, math.Min(d.pieces[0].End, d.max), frac)
	}
	if pieceIdx == pieceLen-1 {
		return interpolate(math.Max(d.pieces[pieceLen-1].Start, d.min), d.max, frac)
	}
	return interpolate(
		math.Max(d.pieces[pieceIdx].Start, d.min),
		math.Min(d.pieces[pieceIdx].End, d.max),
		frac)
}

// Snapshot fetches the snapshot of this distribution atomically
func (d *distribution) Snapshot() *snapshot {
	bdn := make(breakdown, len(d.pieces))
	for i := range bdn {
		bdn[i].bucketPiece = d.pieces[i]
	}
	d.lock.RLock()
	defer d.lock.RUnlock()
	for i := range bdn {
		bdn[i].Count = d.counts[i]
	}
	if d.count == 0 {
		return &snapshot{
			Count:     d.count,
			Breakdown: bdn,
		}
	}
	return &snapshot{
		Min:       d.min,
		Max:       d.max,
		Average:   d.total / float64(d.count),
		Median:    d.calculateMedian(),
		Count:     d.count,
		Breakdown: bdn,
	}

}

// value represents the value of a metric.
type value struct {
	val           reflect.Value
	dist          *distribution
	valType       types.Type
	isValAPointer bool
	isfunc        bool
}

var (
	timePtrType = reflect.TypeOf((*time.Time)(nil))
	timeType    = timePtrType.Elem()
)

// Given a type t from the reflect package, return the corresponding
// types.Type and true if t is a pointer to that type or false otherwise.
func getPrimitiveType(t reflect.Type) (types.Type, bool) {
	switch t {
	case timePtrType:
		return types.Time, true
	case timeType:
		return types.Time, false
	default:
		switch t.Kind() {
		case reflect.Bool:
			return types.Bool, false
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return types.Int, false
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return types.Uint, false
		case reflect.Float32, reflect.Float64:
			return types.Float, false
		case reflect.String:
			return types.String, false
		default:
			panic(panicInvalidMetric)
		}
	}
}

func newValue(spec interface{}) *value {
	capDist, ok := spec.(*Distribution)
	if ok {
		return &value{dist: (*distribution)(capDist), valType: types.Dist}
	}
	dist, ok := spec.(*distribution)
	if ok {
		return &value{dist: dist, valType: types.Dist}
	}
	v := reflect.ValueOf(spec)
	t := v.Type()
	if t.Kind() == reflect.Func {
		funcArgCount := t.NumOut()

		// Our functions have to return exactly one thing
		if funcArgCount != 1 {
			panic(panicBadFunctionReturnTypes)
		}
		valType, isValAPointer := getPrimitiveType(t.Out(0))
		return &value{
			val:           v,
			valType:       valType,
			isfunc:        true,
			isValAPointer: isValAPointer}
	}
	v = v.Elem()
	valType, isValAPointer := getPrimitiveType(v.Type())
	return &value{val: v, valType: valType, isValAPointer: isValAPointer}
}

// Type returns the type of this value: Int, Float, Uint, String, or Dist
func (v *value) Type() types.Type {
	return v.valType
}

func (v *value) evaluate() reflect.Value {
	if !v.isfunc {
		return v.val
	}
	result := v.val.Call(nil)
	return result[0]
}

// AsXXX methods return this value as a type XX.
// AsXXX methods panic if this value is not of type XX.
func (v *value) AsBool() bool {
	if v.valType != types.Bool {
		panic(panicIncompatibleTypes)
	}
	return v.evaluate().Bool()
}

func (v *value) AsInt() int64 {
	if v.valType != types.Int {
		panic(panicIncompatibleTypes)
	}
	return v.evaluate().Int()
}

func (v *value) AsUint() uint64 {
	if v.valType != types.Uint {
		panic(panicIncompatibleTypes)
	}
	return v.evaluate().Uint()
}

func (v *value) AsFloat() float64 {
	if v.valType != types.Float {
		panic(panicIncompatibleTypes)
	}
	return v.evaluate().Float()
}

func (v *value) AsString() string {
	if v.valType != types.String {
		panic(panicIncompatibleTypes)
	}
	return v.evaluate().String()
}

func (v *value) AsTime() (result time.Time) {
	if v.valType != types.Time {
		panic(panicIncompatibleTypes)
	}
	val := v.evaluate()
	if v.isValAPointer {
		p := val.Interface().(*time.Time)
		if p == nil {
			return
		}
		return *p
	}
	return val.Interface().(time.Time)
}

func asRPCRanges(ranges breakdown) []*messages.RangeWithCount {
	result := make([]*messages.RangeWithCount, len(ranges))
	for i := range ranges {
		result[i] = &messages.RangeWithCount{Count: ranges[i].Count}
		if !ranges[i].First {
			result[i].Lower = &ranges[i].Start
		}
		if !ranges[i].Last {
			result[i].Upper = &ranges[i].End
		}
	}
	return result
}

func (v *value) AsRPCValue() *messages.Value {
	t := v.Type()
	switch t {
	case types.Bool:
		b := v.AsBool()
		return &messages.Value{Kind: t, BoolValue: &b}
	case types.Int:
		i := v.AsInt()
		return &messages.Value{Kind: t, IntValue: &i}
	case types.Uint:
		u := v.AsUint()
		return &messages.Value{Kind: t, UintValue: &u}
	case types.Float:
		f := v.AsFloat()
		return &messages.Value{Kind: t, FloatValue: &f}
	case types.String:
		s := v.AsString()
		return &messages.Value{Kind: t, StringValue: &s}
	case types.Time:
		s := v.AsTextString()
		return &messages.Value{Kind: t, StringValue: &s}
	case types.Dist:
		snapshot := v.AsDistribution().Snapshot()
		return &messages.Value{
			Kind: t,
			DistributionValue: &messages.Distribution{
				Min:     snapshot.Min,
				Max:     snapshot.Max,
				Average: snapshot.Average,
				Median:  snapshot.Median,
				Count:   snapshot.Count,
				Ranges:  asRPCRanges(snapshot.Breakdown)}}
	default:
		panic(panicIncompatibleTypes)
	}
}

// AsTextString returns this value as a text friendly string.
// AsTextString panics if this value does not represent a single value.
// For example, AsTextString panics if this value represents a distribution.
func (v *value) AsTextString() string {
	switch v.Type() {
	case types.Bool:
		if v.AsBool() {
			return "true"
		}
		return "false"
	case types.Int:
		return strconv.FormatInt(v.AsInt(), 10)
	case types.Uint:
		return strconv.FormatUint(v.AsUint(), 10)
	case types.Float:
		return strconv.FormatFloat(v.AsFloat(), 'f', -1, 64)
	case types.String:
		return "\"" + v.AsString() + "\""
	case types.Time:
		t := v.AsTime()
		if t.IsZero() {
			return "0.000000000"
		}
		return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
	default:
		panic(panicIncompatibleTypes)
	}
}

// AsHtmlString returns this value as an html friendly string.
// AsHtmlString panics if this value does not represent a single value.
// For example, AsHtmlString panics if this value represents a distribution.
func (v *value) AsHtmlString() string {
	switch v.Type() {
	case types.Time:
		t := v.AsTime().UTC()
		return t.Format("2006-01-02T15:04:05.999999999Z")
	default:
		return v.AsTextString()
	}
}

// AsDistribution returns this value as a Distribution.
// AsDistribution panics if this value does not represent a distribution
func (v *value) AsDistribution() *distribution {
	if v.valType != types.Dist {
		panic(panicIncompatibleTypes)
	}
	return v.dist
}

// metric represents a single metric.
type metric struct {
	// The description of the metric
	Description string
	// The unit of measurement
	Unit units.Unit
	// The value of the metric
	Value              *value
	enclosingListEntry *listEntry
}

// AbsPath returns the absolute path of this metric
func (m *metric) AbsPath() string {
	return m.enclosingListEntry.absPath()
}

// listEntry represents a single entry in a directory listing.
type listEntry struct {
	// The name of this list entry.
	Name string
	// If this list entry represents a metric, Metric is non-nil
	Metric *metric
	// If this list entry represents a directory, Directory is non-nil
	Directory *directory
	parent    *listEntry
}

func (n *listEntry) pathFrom(fromDir *directory) pathSpec {
	var names pathSpec
	current := n
	from := fromDir.enclosingListEntry
	for ; current != nil && current != from; current = current.parent {
		names = append(names, current.Name)
	}
	if current != from {
		return nil
	}
	pathLen := len(names)
	for i := 0; i < pathLen/2; i++ {
		names[i], names[pathLen-i-1] = names[pathLen-i-1], names[i]
	}
	return names
}

func (n *listEntry) absPath() string {
	return "/" + n.pathFrom(root).String()
}

// metricsCollector represents any data structure used to collect metrics.
type metricsCollector interface {
	// Collect collects a single metric.
	Collect(m *metric) error
}

// directory represents a directory same as DirectorySpec
type directory struct {
	contents           map[string]*listEntry
	enclosingListEntry *listEntry
}

func newDirectory() *directory {
	return &directory{contents: make(map[string]*listEntry)}
}

// List lists the contents of this directory in lexographical order by name.
func (d *directory) List() []*listEntry {
	result := make([]*listEntry, len(d.contents))
	idx := 0
	for _, n := range d.contents {
		result[idx] = n
		idx++
	}
	return sortListEntries(result)
}

// AbsPath returns the absolute path of this directory
func (d *directory) AbsPath() string {
	return d.enclosingListEntry.absPath()
}

// GetDirectory returns the directory with the given relative
// path or nil if no such directory exists.
func (d *directory) GetDirectory(relativePath string) *directory {
	return d.getDirectory(newPathSpec(relativePath))
}

// GetMetric returns the metric with the given relative
// path or nil if no such metric exists.
func (d *directory) GetMetric(relativePath string) *metric {
	_, m := d.getDirectoryOrMetric(newPathSpec(relativePath))
	return m
}

// GetAllMetricsByPath does a depth first traversal from relativePath to
// find all the metrics and store them within collector. If relativePath
// denotes a single metric, then GetAllMetricsByPath stores that single metric
// within collector. If relativePath does not exist in this directory, then
// GetAllMetricsByPath stores nothing in collector.
// If the Collect() method of collector returns a non nil error,
// GetAllMetricsByPath stops traversal and returns that same error.
func (d *directory) GetAllMetricsByPath(
	relativePath string, collector metricsCollector) error {
	dir, m := d.GetDirectoryOrMetric(relativePath)
	if m != nil {
		return collector.Collect(m)
	} else if dir != nil {
		return dir.GetAllMetrics(collector)
	}
	return nil
}

// GetDirectoryOrMetric returns either the directory or metric
// at the given path while traversing the directory tree just one time.
// If path not found: returns nil, nil; if path is a directory:
// returns d, nil; if path is a metric: returns nil, m.
func (d *directory) GetDirectoryOrMetric(relativePath string) (
	*directory, *metric) {
	return d.getDirectoryOrMetric(newPathSpec(relativePath))
}

// GetAllMetricsByPath does a depth first traversal of this directory to
// find all the metrics and store them within collector.
// If the Collect() method of collector returns a non nil error,
// GetAllMetrics stops traversal and returns that same error.
func (d *directory) GetAllMetrics(collector metricsCollector) (err error) {
	for _, entry := range d.List() {
		if entry.Directory != nil {
			err = entry.Directory.GetAllMetrics(collector)
		} else {
			err = collector.Collect(entry.Metric)
		}
		if err != nil {
			return
		}
	}
	return
}

func (d *directory) getDirectory(path pathSpec) (result *directory) {
	result = d
	for _, part := range path {
		n := result.contents[part]
		if n == nil || n.Directory == nil {
			return nil
		}
		result = n.Directory
	}
	return
}

func (d *directory) getDirectoryOrMetric(path pathSpec) (
	*directory, *metric) {
	if path.Empty() {
		return d, nil
	}
	dir := d.getDirectory(path.Dir())
	if dir == nil {
		return nil, nil
	}
	n := dir.contents[path.Base()]
	if n == nil {
		return nil, nil
	}
	return n.Directory, n.Metric
}

func (d *directory) createDirIfNeeded(name string) (*directory, error) {
	n := d.contents[name]

	// We need to create the new directory
	if n == nil {
		newDir := newDirectory()
		newListEntry := &listEntry{
			Name: name, Directory: newDir, parent: d.enclosingListEntry}
		newDir.enclosingListEntry = newListEntry
		d.contents[name] = newListEntry
		return newDir, nil
	}

	// The directory already exists
	if n.Directory != nil {
		return n.Directory, nil
	}

	// name already associated with a metric, return error
	return nil, ErrPathInUse
}

func (d *directory) storeMetric(name string, m *metric) error {
	n := d.contents[name]
	// Oops something already stored under name, return error
	if n != nil {
		return ErrPathInUse
	}
	newListEntry := &listEntry{Name: name, Metric: m, parent: d.enclosingListEntry}
	m.enclosingListEntry = newListEntry
	d.contents[name] = newListEntry
	return nil
}

func (d *directory) registerDirectory(path pathSpec) (
	result *directory, err error) {
	result = d
	for _, part := range path {
		result, err = result.createDirIfNeeded(part)
		if err != nil {
			return
		}
	}
	return
}

func (d *directory) registerMetric(
	path pathSpec,
	value interface{},
	unit units.Unit,
	description string) (err error) {
	if path.Empty() {
		return ErrPathInUse
	}
	current, err := d.registerDirectory(path.Dir())
	if err != nil {
		return
	}
	metric := &metric{
		Description: description,
		Unit:        unit,
		Value:       newValue(value)}
	return current.storeMetric(path.Base(), metric)
}

// pathSpec represents a relative path
type pathSpec []string

func newPathSpec(path string) pathSpec {
	parts := strings.Split(path, "/")

	// Filter out empty path parts
	idx := 0
	for i := range parts {
		if strings.TrimSpace(parts[i]) == "" {
			continue
		}
		parts[idx] = parts[i]
		idx++
	}
	return parts[:idx]
}

// Dir returns the directory part of the path
// Dir panics if this path is empty
func (p pathSpec) Dir() pathSpec {
	plen := len(p)
	if plen == 0 {
		panic("Can't take Dir() of empty path")
	}
	return p[:plen-1]
}

// Base returns the name part of the path
// Base panics if this path is empty
func (p pathSpec) Base() string {
	plen := len(p)
	if plen == 0 {
		panic("Can't take Base() of empty path")
	}
	return p[plen-1]
}

// Empty returns true if this path is empty
func (p pathSpec) Empty() bool {
	return len(p) == 0
}

func (p pathSpec) String() string {
	return strings.Join(p, "/")
}

// byName sorts list entries by name
type byName []*listEntry

func (b byName) Len() int {
	return len(b)
}

func (b byName) Less(i, j int) bool {
	return b[i].Name < b[j].Name
}

func (b byName) Swap(i, j int) {
	b[j], b[i] = b[i], b[j]
}

func sortListEntries(listEntries []*listEntry) []*listEntry {
	sort.Sort(byName(listEntries))
	return listEntries
}