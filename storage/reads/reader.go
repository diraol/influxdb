package reads

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/gogo/protobuf/types"
	"github.com/influxdata/flux"
	"github.com/influxdata/flux/execute"
	"github.com/influxdata/flux/memory"
	"github.com/influxdata/flux/values"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/query/stdlib/influxdata/influxdb"
	"github.com/influxdata/influxdb/storage/reads/datatypes"
	"github.com/influxdata/influxdb/tsdb/cursors"
)

type storageTable interface {
	flux.Table
	Close()
	Cancel()
	Statistics() cursors.CursorStats
}

type storeReader struct {
	s Store
}

func NewReader(s Store) influxdb.Reader {
	return &storeReader{s: s}
}

func (r *storeReader) Read(ctx context.Context, rs influxdb.ReadSpec, start, stop execute.Time, alloc *memory.Allocator) (influxdb.TableIterator, error) {
	var predicate *datatypes.Predicate
	if rs.Predicate != nil {
		p, err := toStoragePredicate(rs.Predicate)
		if err != nil {
			return nil, err
		}
		predicate = p
	}

	return &tableIterator{
		ctx:       ctx,
		bounds:    execute.Bounds{Start: start, Stop: stop},
		s:         r.s,
		readSpec:  rs,
		predicate: predicate,
		alloc:     alloc,
	}, nil
}

func (r *storeReader) ReadFilter(ctx context.Context, spec influxdb.ReadFilterSpec, alloc *memory.Allocator) (influxdb.TableIterator, error) {
	return &simpleTableIterator{
		ctx:   ctx,
		s:     r.s,
		spec:  spec,
		alloc: alloc,
	}, nil
}

func (r *storeReader) ReadTagKeys(ctx context.Context, spec influxdb.ReadTagKeysSpec, alloc *memory.Allocator) (influxdb.TableIterator, error) {
	var predicate *datatypes.Predicate
	if spec.Predicate != nil {
		p, err := toStoragePredicate(spec.Predicate)
		if err != nil {
			return nil, err
		}
		predicate = p
	}

	return &tagKeysIterator{
		ctx:       ctx,
		bounds:    spec.Bounds,
		s:         r.s,
		readSpec:  spec,
		predicate: predicate,
		alloc:     alloc,
	}, nil
}

func (r *storeReader) ReadTagValues(ctx context.Context, spec influxdb.ReadTagValuesSpec, alloc *memory.Allocator) (influxdb.TableIterator, error) {
	var predicate *datatypes.Predicate
	if spec.Predicate != nil {
		p, err := toStoragePredicate(spec.Predicate)
		if err != nil {
			return nil, err
		}
		predicate = p
	}

	return &tagValuesIterator{
		ctx:       ctx,
		bounds:    spec.Bounds,
		s:         r.s,
		readSpec:  spec,
		predicate: predicate,
		alloc:     alloc,
	}, nil
}

func (r *storeReader) Close() {}

type simpleTableIterator struct {
	ctx   context.Context
	s     Store
	spec  influxdb.ReadFilterSpec
	stats cursors.CursorStats
	alloc *memory.Allocator
}

func (bi *simpleTableIterator) Statistics() cursors.CursorStats { return bi.stats }

func (bi *simpleTableIterator) Do(f func(flux.Table) error) error {
	src := bi.s.GetSource(
		uint64(bi.spec.OrganizationID),
		uint64(bi.spec.BucketID),
	)

	// Setup read request
	any, err := types.MarshalAny(src)
	if err != nil {
		return err
	}

	var predicate *datatypes.Predicate
	if bi.spec.Predicate != nil {
		p, err := toStoragePredicate(bi.spec.Predicate)
		if err != nil {
			return err
		}
		predicate = p
	}

	var req datatypes.ReadFilterRequest
	req.ReadSource = any
	req.Predicate = predicate
	req.Range.Start = int64(bi.spec.Bounds.Start)
	req.Range.End = int64(bi.spec.Bounds.Stop)

	rs, err := bi.s.ReadFilter(bi.ctx, &req)
	if err != nil {
		return err
	}

	if rs == nil {
		return nil
	}

	return bi.handleRead(f, rs)
}

func (bi *simpleTableIterator) handleRead(f func(flux.Table) error, rs ResultSet) error {
	// these resources must be closed if not nil on return
	var (
		cur   cursors.Cursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if cur != nil {
			cur.Close()
		}
		rs.Close()
	}()

READ:
	for rs.Next() {
		cur = rs.Cursor()
		if cur == nil {
			// no data for series key + field combination
			continue
		}

		bnds := bi.spec.Bounds
		key := defaultGroupKeyForSeries(rs.Tags(), bnds)
		done := make(chan struct{})
		switch typedCur := cur.(type) {
		case cursors.IntegerArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TInt)
			table = newIntegerTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.FloatArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TFloat)
			table = newFloatTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.UnsignedArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TUInt)
			table = newUnsignedTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.BooleanArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TBool)
			table = newBooleanTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.StringArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TString)
			table = newStringTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, bi.alloc)
		default:
			panic(fmt.Sprintf("unreachable: %T", typedCur))
		}

		cur = nil

		if !table.Empty() {
			if err := f(table); err != nil {
				table.Close()
				table = nil
				return err
			}
			select {
			case <-done:
			case <-bi.ctx.Done():
				table.Cancel()
				break READ
			}
		}

		stats := table.Statistics()
		bi.stats.ScannedValues += stats.ScannedValues
		bi.stats.ScannedBytes += stats.ScannedBytes
		table.Close()
		table = nil
	}
	return rs.Err()
}

type tableIterator struct {
	ctx       context.Context
	bounds    execute.Bounds
	s         Store
	readSpec  influxdb.ReadSpec
	predicate *datatypes.Predicate
	stats     cursors.CursorStats
	alloc     *memory.Allocator
}

func (bi *tableIterator) Statistics() cursors.CursorStats { return bi.stats }

func (bi *tableIterator) Do(f func(flux.Table) error) error {
	src := bi.s.GetSource(
		uint64(bi.readSpec.OrganizationID),
		uint64(bi.readSpec.BucketID),
	)

	// Setup read request
	var req datatypes.ReadRequest
	if any, err := types.MarshalAny(src); err != nil {
		return err
	} else {
		req.ReadSource = any
	}
	req.Predicate = bi.predicate
	req.Descending = bi.readSpec.Descending
	req.TimestampRange.Start = int64(bi.bounds.Start)
	req.TimestampRange.End = int64(bi.bounds.Stop)
	req.Group = convertGroupMode(bi.readSpec.GroupMode)
	req.GroupKeys = bi.readSpec.GroupKeys
	req.SeriesLimit = bi.readSpec.SeriesLimit
	req.PointsLimit = bi.readSpec.PointsLimit
	req.SeriesOffset = bi.readSpec.SeriesOffset

	if req.PointsLimit == -1 {
		req.Hints.SetNoPoints()
	}

	if agg, err := determineAggregateMethod(bi.readSpec.AggregateMethod); err != nil {
		return err
	} else if agg != datatypes.AggregateTypeNone {
		req.Aggregate = &datatypes.Aggregate{Type: agg}
	}

	switch {
	case req.Group != datatypes.GroupAll:
		rs, err := bi.s.GroupRead(bi.ctx, &req)
		if err != nil {
			return err
		}

		if rs == nil {
			return nil
		}

		if req.Hints.NoPoints() {
			return bi.handleGroupReadNoPoints(f, rs)
		}
		return bi.handleGroupRead(f, rs)

	default:
		rs, err := bi.s.Read(bi.ctx, &req)
		if err != nil {
			return err
		}

		if rs == nil {
			return nil
		}

		if req.Hints.NoPoints() {
			return bi.handleReadNoPoints(f, rs)
		}
		return bi.handleRead(f, rs)
	}
}

func (bi *tableIterator) handleRead(f func(flux.Table) error, rs ResultSet) error {
	// these resources must be closed if not nil on return
	var (
		cur   cursors.Cursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if cur != nil {
			cur.Close()
		}
		rs.Close()
	}()

READ:
	for rs.Next() {
		cur = rs.Cursor()
		if cur == nil {
			// no data for series key + field combination
			continue
		}

		key := groupKeyForSeries(rs.Tags(), &bi.readSpec, bi.bounds)
		done := make(chan struct{})
		switch typedCur := cur.(type) {
		case cursors.IntegerArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TInt)
			table = newIntegerTable(done, typedCur, bi.bounds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.FloatArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TFloat)
			table = newFloatTable(done, typedCur, bi.bounds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.UnsignedArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TUInt)
			table = newUnsignedTable(done, typedCur, bi.bounds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.BooleanArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TBool)
			table = newBooleanTable(done, typedCur, bi.bounds, key, cols, rs.Tags(), defs, bi.alloc)
		case cursors.StringArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TString)
			table = newStringTable(done, typedCur, bi.bounds, key, cols, rs.Tags(), defs, bi.alloc)
		default:
			panic(fmt.Sprintf("unreachable: %T", typedCur))
		}

		cur = nil

		if !table.Empty() {
			if err := f(table); err != nil {
				table.Close()
				table = nil
				return err
			}
			select {
			case <-done:
			case <-bi.ctx.Done():
				table.Cancel()
				break READ
			}
		}

		stats := table.Statistics()
		bi.stats.ScannedValues += stats.ScannedValues
		bi.stats.ScannedBytes += stats.ScannedBytes
		table.Close()
		table = nil
	}
	return rs.Err()
}

func (bi *tableIterator) handleReadNoPoints(f func(flux.Table) error, rs ResultSet) error {
	// these resources must be closed if not nil on return
	var table storageTable

	defer func() {
		if table != nil {
			table.Close()
		}
		rs.Close()
	}()

READ:
	for rs.Next() {
		cur := rs.Cursor()
		if !hasPoints(cur) {
			// no data for series key + field combination
			continue
		}

		key := groupKeyForSeries(rs.Tags(), &bi.readSpec, bi.bounds)
		done := make(chan struct{})
		cols, defs := determineTableColsForSeries(rs.Tags(), flux.TString)
		table = newTableNoPoints(done, bi.bounds, key, cols, rs.Tags(), defs, bi.alloc)

		if err := f(table); err != nil {
			table.Close()
			table = nil
			return err
		}
		select {
		case <-done:
		case <-bi.ctx.Done():
			table.Cancel()
			break READ
		}

		table.Close()
		table = nil
	}
	return rs.Err()
}

func (bi *tableIterator) handleGroupRead(f func(flux.Table) error, rs GroupResultSet) error {
	// these resources must be closed if not nil on return
	var (
		gc    GroupCursor
		cur   cursors.Cursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if cur != nil {
			cur.Close()
		}
		if gc != nil {
			gc.Close()
		}
		rs.Close()
	}()

	gc = rs.Next()
READ:
	for gc != nil {
		for gc.Next() {
			cur = gc.Cursor()
			if cur != nil {
				break
			}
		}

		if cur == nil {
			gc.Close()
			gc = rs.Next()
			continue
		}

		key := groupKeyForGroup(gc.PartitionKeyVals(), &bi.readSpec, bi.bounds)
		done := make(chan struct{})
		switch typedCur := cur.(type) {
		case cursors.IntegerArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TInt)
			table = newIntegerGroupTable(done, gc, typedCur, bi.bounds, key, cols, gc.Tags(), defs, bi.alloc)
		case cursors.FloatArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TFloat)
			table = newFloatGroupTable(done, gc, typedCur, bi.bounds, key, cols, gc.Tags(), defs, bi.alloc)
		case cursors.UnsignedArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TUInt)
			table = newUnsignedGroupTable(done, gc, typedCur, bi.bounds, key, cols, gc.Tags(), defs, bi.alloc)
		case cursors.BooleanArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TBool)
			table = newBooleanGroupTable(done, gc, typedCur, bi.bounds, key, cols, gc.Tags(), defs, bi.alloc)
		case cursors.StringArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TString)
			table = newStringGroupTable(done, gc, typedCur, bi.bounds, key, cols, gc.Tags(), defs, bi.alloc)
		default:
			panic(fmt.Sprintf("unreachable: %T", typedCur))
		}

		// table owns these resources and is responsible for closing them
		cur = nil
		gc = nil

		if err := f(table); err != nil {
			table.Close()
			table = nil
			return err
		}
		select {
		case <-done:
		case <-bi.ctx.Done():
			table.Cancel()
			break READ
		}

		stats := table.Statistics()
		bi.stats.ScannedValues += stats.ScannedValues
		bi.stats.ScannedBytes += stats.ScannedBytes
		table.Close()
		table = nil

		gc = rs.Next()
	}
	return rs.Err()
}

func (bi *tableIterator) handleGroupReadNoPoints(f func(flux.Table) error, rs GroupResultSet) error {
	// these resources must be closed if not nil on return
	var (
		gc    GroupCursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if gc != nil {
			gc.Close()
		}
		rs.Close()
	}()

	gc = rs.Next()
READ:
	for gc != nil {
		key := groupKeyForGroup(gc.PartitionKeyVals(), &bi.readSpec, bi.bounds)
		done := make(chan struct{})
		cols, defs := determineTableColsForGroup(gc.Keys(), flux.TString)
		table = newGroupTableNoPoints(done, bi.bounds, key, cols, defs, bi.alloc)
		gc.Close()
		gc = nil

		if err := f(table); err != nil {
			table.Close()
			table = nil
			return err
		}
		select {
		case <-done:
		case <-bi.ctx.Done():
			table.Cancel()
			break READ
		}

		table.Close()
		table = nil

		gc = rs.Next()
	}
	return rs.Err()
}

func determineAggregateMethod(agg string) (datatypes.Aggregate_AggregateType, error) {
	if agg == "" {
		return datatypes.AggregateTypeNone, nil
	}

	if t, ok := datatypes.Aggregate_AggregateType_value[strings.ToUpper(agg)]; ok {
		return datatypes.Aggregate_AggregateType(t), nil
	}
	return 0, fmt.Errorf("unknown aggregate type %q", agg)
}

func convertGroupMode(m influxdb.GroupMode) datatypes.ReadRequest_Group {
	switch m {
	case influxdb.GroupModeNone:
		return datatypes.GroupNone
	case influxdb.GroupModeBy:
		return datatypes.GroupBy
	case influxdb.GroupModeExcept:
		return datatypes.GroupExcept

	case influxdb.GroupModeDefault, influxdb.GroupModeAll:
		fallthrough
	default:
		return datatypes.GroupAll
	}
}

const (
	startColIdx = 0
	stopColIdx  = 1
	timeColIdx  = 2
	valueColIdx = 3
)

func determineTableColsForSeries(tags models.Tags, typ flux.ColType) ([]flux.ColMeta, [][]byte) {
	cols := make([]flux.ColMeta, 4+len(tags))
	defs := make([][]byte, 4+len(tags))
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	cols[timeColIdx] = flux.ColMeta{
		Label: execute.DefaultTimeColLabel,
		Type:  flux.TTime,
	}
	cols[valueColIdx] = flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  typ,
	}
	for j, tag := range tags {
		cols[4+j] = flux.ColMeta{
			Label: string(tag.Key),
			Type:  flux.TString,
		}
		defs[4+j] = []byte("")
	}
	return cols, defs
}

func defaultGroupKeyForSeries(tags models.Tags, bnds execute.Bounds) flux.GroupKey {
	cols := make([]flux.ColMeta, 2, len(tags))
	vs := make([]values.Value, 2, len(tags))
	cols[0] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	vs[0] = values.NewTime(bnds.Start)
	cols[1] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	vs[1] = values.NewTime(bnds.Stop)
	for i := range tags {
		cols = append(cols, flux.ColMeta{
			Label: string(tags[i].Key),
			Type:  flux.TString,
		})
		vs = append(vs, values.NewString(string(tags[i].Value)))
	}
	return execute.NewGroupKey(cols, vs)
}

func groupKeyForSeries(tags models.Tags, readSpec *influxdb.ReadSpec, bnds execute.Bounds) flux.GroupKey {
	cols := make([]flux.ColMeta, 2, len(tags))
	vs := make([]values.Value, 2, len(tags))
	cols[0] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	vs[0] = values.NewTime(bnds.Start)
	cols[1] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	vs[1] = values.NewTime(bnds.Stop)
	switch readSpec.GroupMode {
	case influxdb.GroupModeBy:
		// group key in GroupKeys order, including tags in the GroupKeys slice
		for _, k := range readSpec.GroupKeys {
			bk := []byte(k)
			for _, t := range tags {
				if bytes.Equal(t.Key, bk) && len(t.Value) > 0 {
					cols = append(cols, flux.ColMeta{
						Label: k,
						Type:  flux.TString,
					})
					vs = append(vs, values.NewString(string(t.Value)))
				}
			}
		}
	case influxdb.GroupModeExcept:
		// group key in GroupKeys order, skipping tags in the GroupKeys slice
		panic("not implemented")
	case influxdb.GroupModeDefault, influxdb.GroupModeAll:
		for i := range tags {
			cols = append(cols, flux.ColMeta{
				Label: string(tags[i].Key),
				Type:  flux.TString,
			})
			vs = append(vs, values.NewString(string(tags[i].Value)))
		}
	}
	return execute.NewGroupKey(cols, vs)
}

func determineTableColsForGroup(tagKeys [][]byte, typ flux.ColType) ([]flux.ColMeta, [][]byte) {
	cols := make([]flux.ColMeta, 4+len(tagKeys))
	defs := make([][]byte, 4+len(tagKeys))
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	cols[timeColIdx] = flux.ColMeta{
		Label: execute.DefaultTimeColLabel,
		Type:  flux.TTime,
	}
	cols[valueColIdx] = flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  typ,
	}
	for j, tag := range tagKeys {
		cols[4+j] = flux.ColMeta{
			Label: string(tag),
			Type:  flux.TString,
		}
		defs[4+j] = []byte("")

	}
	return cols, defs
}

func groupKeyForGroup(kv [][]byte, readSpec *influxdb.ReadSpec, bnds execute.Bounds) flux.GroupKey {
	cols := make([]flux.ColMeta, 2, len(readSpec.GroupKeys)+2)
	vs := make([]values.Value, 2, len(readSpec.GroupKeys)+2)
	cols[0] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	vs[0] = values.NewTime(bnds.Start)
	cols[1] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	vs[1] = values.NewTime(bnds.Stop)
	for i := range readSpec.GroupKeys {
		if readSpec.GroupKeys[i] == execute.DefaultStartColLabel || readSpec.GroupKeys[i] == execute.DefaultStopColLabel {
			continue
		}
		cols = append(cols, flux.ColMeta{
			Label: readSpec.GroupKeys[i],
			Type:  flux.TString,
		})
		vs = append(vs, values.NewString(string(kv[i])))
	}
	return execute.NewGroupKey(cols, vs)
}

type tagKeysIterator struct {
	ctx       context.Context
	bounds    execute.Bounds
	s         Store
	readSpec  influxdb.ReadTagKeysSpec
	predicate *datatypes.Predicate
	alloc     *memory.Allocator
}

func (ti *tagKeysIterator) Do(f func(flux.Table) error) error {
	src := ti.s.GetSource(
		uint64(ti.readSpec.OrganizationID),
		uint64(ti.readSpec.BucketID),
	)

	var req datatypes.TagKeysRequest
	if any, err := types.MarshalAny(src); err != nil {
		return err
	} else {
		req.TagsSource = any
	}
	req.Predicate = ti.predicate
	req.Range.Start = int64(ti.bounds.Start)
	req.Range.End = int64(ti.bounds.Stop)

	rs, err := ti.s.TagKeys(ti.ctx, &req)
	if err != nil {
		return err
	}
	return ti.handleRead(f, rs)
}

func (ti *tagKeysIterator) handleRead(f func(flux.Table) error, rs cursors.StringIterator) error {
	key := execute.NewGroupKey(nil, nil)
	builder := execute.NewColListTableBuilder(key, ti.alloc)
	valueIdx, err := builder.AddCol(flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  flux.TString,
	})
	if err != nil {
		return err
	}
	defer builder.ClearData()

	for rs.Next() {
		if err := builder.AppendString(valueIdx, rs.Value()); err != nil {
			return err
		}
	}

	// Construct the table and add to the reference count
	// so we can free the table later.
	tbl, err := builder.Table()
	if err != nil {
		return err
	}
	tbl.RefCount(1)
	// TODO(jsternberg): We do not properly handle reference counts
	// in the query engine so even though we should release our reference
	// count, we cannot since the function may take ownership of the data
	// without telling us.
	// defer tbl.RefCount(-1)

	// Release the references to the arrays held by the builder.
	builder.ClearData()
	return f(tbl)
}

func (ti *tagKeysIterator) Statistics() cursors.CursorStats {
	return cursors.CursorStats{}
}

type tagValuesIterator struct {
	ctx       context.Context
	bounds    execute.Bounds
	s         Store
	readSpec  influxdb.ReadTagValuesSpec
	predicate *datatypes.Predicate
	alloc     *memory.Allocator
}

func (ti *tagValuesIterator) Do(f func(flux.Table) error) error {
	src := ti.s.GetSource(
		uint64(ti.readSpec.OrganizationID),
		uint64(ti.readSpec.BucketID),
	)

	var req datatypes.TagValuesRequest
	if any, err := types.MarshalAny(src); err != nil {
		return err
	} else {
		req.TagsSource = any
	}
	switch ti.readSpec.TagKey {
	case "_measurement":
		req.TagKey = models.MeasurementTagKey
	case "_field":
		req.TagKey = models.FieldKeyTagKey
	default:
		req.TagKey = ti.readSpec.TagKey
	}
	req.Predicate = ti.predicate
	req.Range.Start = int64(ti.bounds.Start)
	req.Range.End = int64(ti.bounds.Stop)

	rs, err := ti.s.TagValues(ti.ctx, &req)
	if err != nil {
		return err
	}
	return ti.handleRead(f, rs)
}

func (ti *tagValuesIterator) handleRead(f func(flux.Table) error, rs cursors.StringIterator) error {
	key := execute.NewGroupKey(nil, nil)
	builder := execute.NewColListTableBuilder(key, ti.alloc)
	valueIdx, err := builder.AddCol(flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  flux.TString,
	})
	if err != nil {
		return err
	}
	defer builder.ClearData()

	for rs.Next() {
		if err := builder.AppendString(valueIdx, rs.Value()); err != nil {
			return err
		}
	}

	// Construct the table and add to the reference count
	// so we can free the table later.
	tbl, err := builder.Table()
	if err != nil {
		return err
	}
	tbl.RefCount(1)
	// TODO(jsternberg): We do not properly handle reference counts
	// in the query engine so even though we should release our reference
	// count, we cannot since the function may take ownership of the data
	// without telling us.
	// defer tbl.RefCount(-1)

	// Release the references to the arrays held by the builder.
	builder.ClearData()
	return f(tbl)
}

func (ti *tagValuesIterator) Statistics() cursors.CursorStats {
	return cursors.CursorStats{}
}
