package trace

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	opentracing "github.com/opentracing/opentracing-go"
	opentracinglog "github.com/opentracing/opentracing-go/log"
	"github.com/stripe/veneur/ssf"
)

var _ opentracing.Tracer = &Tracer{}
var _ opentracing.Span = &Span{}
var _ opentracing.SpanContext = &spanContext{}
var _ opentracing.StartSpanOption = &spanOption{}
var _ opentracing.TextMapReader = textMapReaderWriter(map[string]string{})
var _ opentracing.TextMapWriter = textMapReaderWriter(map[string]string{})

var ErrUnsupportedSpanContext = errors.New("Unsupported SpanContext")

type ErrContractViolation struct {
	details interface{}
}

func (e ErrContractViolation) Error() string {
	return fmt.Sprintf("Contract violation: %s: %#v", e.details)
}

type textMapReaderWriter map[string]string

func (t textMapReaderWriter) ForeachKey(handler func(k, v string) error) error {
	for k, v := range t {
		err := handler(k, v)
		if err != nil {
			return err
		}
	}
	return nil
}

func (t textMapReaderWriter) Set(k, v string) {
	t[k] = v
}

// Clone creates a new textMapReaderWriter with the same
// key-value pairs
func (t textMapReaderWriter) Clone() textMapReaderWriter {
	clone := textMapReaderWriter(map[string]string{})
	t.CloneTo(clone)
	return clone
}

// CloneTo clones the textMapReaderWriter into the provided TextMapWriter
func (t textMapReaderWriter) CloneTo(w opentracing.TextMapWriter) {
	t.ForeachKey(func(k, v string) error {
		w.Set(k, v)
		return nil
	})
}

type spanContext struct {
	baggageItems map[string]string
}

func (c *spanContext) Init() {
	c.baggageItems = map[string]string{}
}

// ForeachBaggageItem calls the handler function on each key/val pair in
// the spanContext's baggage items. If the handler function returns false, it
// terminates iteration immediately.
func (c *spanContext) ForeachBaggageItem(handler func(k, v string) bool) {
	errHandler := func(k, v string) error {
		b := handler(k, v)
		if !b {
			return errors.New("dummy error")
		}
		return nil
	}

	textMapReaderWriter(c.baggageItems).ForeachKey(errHandler)
}

// TraceID extracts the Trace ID from the BaggageItems.
// It assumes the TraceID is present and valid.
func (c *spanContext) TraceId() int64 {
	return c.parseBaggageInt64("traceid")
}

// ParentID extracts the Parent ID from the BaggageItems.
// It assumes the ParentID is present and valid.
func (c *spanContext) ParentId() int64 {
	return c.parseBaggageInt64("parentid")
}

// SpanId extracts the Span ID from the BaggageItems.
// It assumes the SpanId is present and valid.
func (c *spanContext) SpanId() int64 {
	return c.parseBaggageInt64("spanid")
}

// parseBaggageInt64 searches for the target key in the BaggageItems
// and parses it as an int64. It treats keys as case-insensitive.
func (c *spanContext) parseBaggageInt64(key string) int64 {
	var val int64
	c.ForeachBaggageItem(func(k, v string) bool {
		if strings.ToLower(k) == strings.ToLower(key) {
			i, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				// TODO handle err
				return true
			}
			val = i
			return false
		}
		return true
	})
	return val
}

// Resource returns the resource assocaited with the spanContext
func (c *spanContext) Resource() string {
	var resource string
	c.ForeachBaggageItem(func(k, v string) bool {
		if strings.ToLower(k) == "resource" {
			resource = v
			return false
		}
		return true
	})
	return resource
}

type Span struct {
	tracer Tracer

	*Trace

	// These are currently ignored
	logLines []opentracinglog.Field
}

func (s *Span) Finish() {
	s.FinishWithOptions(opentracing.FinishOptions{
		FinishTime:  time.Now(),
		LogRecords:  nil,
		BulkLogData: nil,
	})

}

// FinishWithOptions finishes the span, but with explicit
// control over timestamps and log data.
// The BulkLogData field is deprecated and ignored.
func (s *Span) FinishWithOptions(opts opentracing.FinishOptions) {
}

func (s *Span) Context() opentracing.SpanContext {
	return s.contextAsParent()
}

// contextAsParent() is like its exported counterpart,
// except it returns the concrete type for local package use
func (s *Span) contextAsParent() *spanContext {
	//TODO baggageItems

	c := &spanContext{}
	c.Init()
	c.baggageItems["traceid"] = strconv.FormatInt(s.TraceId, 10)
	c.baggageItems["parentid"] = strconv.FormatInt(s.ParentId, 10)
	c.baggageItems["resource"] = s.Resource
	return c
}

func (s *Span) SetOperationName(name string) opentracing.Span {
	s.Trace.Resource = name
	return s
}

// SetTag sets the tags on the underlying span
func (s *Span) SetTag(key string, value interface{}) opentracing.Span {
	tag := ssf.SSFTag{Name: key}
	// TODO mutex
	switch v := value.(type) {
	case string:
		tag.Value = v
	case fmt.Stringer:
		tag.Value = v.String()
	default:
		// TODO maybe just ban non-strings?
		tag.Value = fmt.Sprintf("%#v", value)
	}
	s.Tags = append(s.Tags, &tag)
	return s
}

// LogFields sets log fields on the underlying span.
// Currently these are ignored, but they can be fun to set anyway!
func (s *Span) LogFields(fields ...opentracinglog.Field) {
	// TODO mutex this
	s.logLines = append(s.logLines, fields...)
}

func (s *Span) LogKV(alternatingKeyValues ...interface{}) {
	// TODO handle error
	fs, _ := opentracinglog.InterleavedKVToFields(alternatingKeyValues...)
	s.LogFields(fs...)
}

func (s *Span) SetBaggageItem(restrictedKey, value string) opentracing.Span {
	s.contextAsParent().baggageItems[restrictedKey] = value
	return s
}

func (s *Span) BaggageItem(restrictedKey string) string {
	return s.contextAsParent().baggageItems[restrictedKey]
}

// Tracer returns the tracer that created this Span
func (s *Span) Tracer() opentracing.Tracer {
	return s.tracer
}

// LogEvent is deprecated and unimplemented.
// It is included only to satisfy the opentracing.Span interface.
func (s *Span) LogEvent(event string) {
}

// LogEventWithPayload is deprecated and unimplemented.
// It is included only to satisfy the opentracing.Span interface.
func (s *Span) LogEventWithPayload(event string, payload interface{}) {
}

// Log is deprecated and unimplemented.
// It is included only to satisfy the opentracing.Span interface.
func (s *Span) Log(data opentracing.LogData) {
}

type Tracer struct {
}

type spanOption struct {
	apply func(*opentracing.StartSpanOptions)
}

func (so *spanOption) Apply(sso *opentracing.StartSpanOptions) {
	so.apply(sso)
}

// customSpanStart returns a StartSpanOption that can be passed to
// StartSpan, and which will set the created Span's StartTime to the specified
// value.
func customSpanStart(t time.Time) opentracing.StartSpanOption {
	return &spanOption{
		apply: func(sso *opentracing.StartSpanOptions) {
			sso.StartTime = t
		},
	}
}

func customSpanTags(k, v string) opentracing.StartSpanOption {
	return &spanOption{
		apply: func(sso *opentracing.StartSpanOptions) {
			if sso.Tags == nil {
				sso.Tags = map[string]interface{}{}
			}
			sso.Tags[k] = v
		},
	}
}

func customSpanParent(t *Trace) opentracing.StartSpanOption {
	return &spanOption{
		apply: func(sso *opentracing.StartSpanOptions) {
			sso.References = append(sso.References, opentracing.SpanReference{
				Type:              opentracing.ChildOfRef,
				ReferencedContext: t.contextAsParent(),
			})
		},
	}
}

// StartSpan starts a span with the specified operationName (resource) and options.
// If the options specify a parent span and/or root trace, the resource from the
// root trace will be used.
func (t Tracer) StartSpan(operationName string, opts ...opentracing.StartSpanOption) opentracing.Span {
	// TODO implement References

	sso := opentracing.StartSpanOptions{
		Tags: map[string]interface{}{},
	}
	for _, o := range opts {
		o.Apply(&sso)
	}

	if len(sso.References) == 0 {
		// This is a root-level span
		// beginning a new trace
		return &Span{
			Trace:  StartTrace(operationName),
			tracer: t,
		}
	} else {

		// First, let's extract the parent's information
		parent := Trace{}

		// TODO don't assume that the ReferencedContext is a concrete spanContext
		for _, ref := range sso.References {
			// at the moment, I believe Datadog treats children and follow-children
			// the same way
			switch ref.Type {
			case opentracing.FollowsFromRef:
				fallthrough
			case opentracing.ChildOfRef:
				ctx, ok := ref.ReferencedContext.(*spanContext)
				if !ok {
					continue
				}
				parent.TraceId = ctx.TraceId()
				parent.SpanId = ctx.ParentId()
				parent.Resource = ctx.Resource()

			default:
				// TODO handle error
			}
		}

		// TODO allow us to start the trace as a separate operation
		// to prevent measurement error in timing
		trace := StartChildSpan(&parent)

		if !sso.StartTime.IsZero() {
			trace.Start = sso.StartTime
		}

		span := &Span{
			Trace:  trace,
			tracer: t,
		}

		for k, v := range sso.Tags {
			span.SetTag(k, v)
		}
		return span
	}
}

// Inject injects the provided SpanContext into the carrier for propagation.
// It will return opentracing.ErrUnsupportedFormat if the format is not supported.
// TODO support other SpanContext implementations
// TODO support all the BuiltinFormats
func (t Tracer) Inject(sm opentracing.SpanContext, format interface{}, carrier interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// TODO annotate this error type
			err = ErrContractViolation{r}
		}
	}()

	sc, ok := sm.(*spanContext)
	if !ok {
		return ErrUnsupportedSpanContext
	}

	if format == opentracing.Binary {
		// carrier is guaranteed to be an io.Writer by contract
		w := carrier.(io.Writer)

		trace := &Trace{
			TraceId:  sc.TraceId(),
			ParentId: sc.ParentId(),
			SpanId:   sc.SpanId(),
			Resource: sc.Resource(),
		}

		return trace.ProtoMarshalTo(w)
	}

	// If the carrier is a TextMapWriter, treat it as one, regardless of what the format is
	if w, ok := carrier.(opentracing.TextMapWriter); ok {

		textMapReaderWriter(sc.baggageItems).CloneTo(w)
		return nil
	}

	return opentracing.ErrUnsupportedFormat
}

// Extract returns a SpanContext given the format and the carrier.
// The SpanContext returned represents the parent span (ie, SpanId refers to the parent span's own SpanId).
// TODO support all the BuiltinFormats
func (t Tracer) Extract(format interface{}, carrier interface{}) (ctx opentracing.SpanContext, err error) {
	defer func() {
		if r := recover(); r != nil {
			// TODO annotate this error type
			err = ErrContractViolation{r}
		}
	}()

	if format == opentracing.Binary {
		// carrier is guaranteed to be an io.Reader by contract
		r := carrier.(io.Reader)
		packet, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, err
		}

		sample := ssf.SSFSample{}
		err = proto.Unmarshal(packet, &sample)
		if err != nil {
			return nil, err
		}

		trace := &Trace{
			TraceId:  sample.Trace.TraceId,
			ParentId: sample.Trace.ParentId,
			SpanId:   sample.Trace.Id,
			Resource: sample.Trace.Resource,
		}

		return trace.context(), nil
	}

	if tm, ok := carrier.(opentracing.TextMapReader); ok {

		// carrier is guaranteed to be an opentracing.TextMapReader by contract
		// TODO support other TextMapReader implementations
		traceId, err := strconv.ParseInt(textMapReaderGet(tm, "traceid"), 10, 64)
		spanId, err2 := strconv.ParseInt(textMapReaderGet(tm, "spanid"), 10, 64)
		parentId, err3 := strconv.ParseInt(textMapReaderGet(tm, "parentid"), 10, 64)
		if !(err == nil && err2 == nil && err3 == nil) {
			return nil, errors.New("error parsing fields from TextMapReader")
		}

		trace := &Trace{
			TraceId:  traceId,
			SpanId:   spanId,
			ParentId: parentId,
			Resource: textMapReaderGet(tm, "resource"),
		}
		return trace.context(), nil

	}

	return nil, opentracing.ErrUnsupportedFormat
}

func textMapReaderGet(tmr opentracing.TextMapReader, key string) (value string) {
	tmr.ForeachKey(func(k, v string) error {
		if strings.ToLower(key) == strings.ToLower(k) {
			value = v
			// terminate early by returning an error
			return errors.New("dummy")
		}
		return nil
	})
	return value
}
