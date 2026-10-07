package harness

import (
	"reflect"
	"strings"
	"testing"
)

var notCompared = map[string]string{
	"captured_at":  "two runs are never at the same instant; that is not what makes them different",
	"brw_version":  "comparing one brw version against another is the point of keeping records",
	"go_version":   "part of the build under measurement, like brw_version",
	"cdp_protocol": "follows the browser build, which is compared",
}

func TestComparableClassifiesEveryEnvironmentField(t *testing.T) {
	compared := map[string]bool{}
	for _, name := range ComparedFields {
		compared[name] = true
	}

	structType := reflect.TypeOf(Environment{})
	for i := 0; i < structType.NumField(); i++ {
		name := jsonName(structType.Field(i))
		if compared[name] && notCompared[name] != "" {
			t.Errorf("field %q is both compared and excused", name)
			continue
		}
		if !compared[name] && notCompared[name] == "" {
			t.Errorf("field %q is in neither ComparedFields nor notCompared; decide whether two runs differing in it are comparable", name)
		}
	}

	for name := range notCompared {
		if !hasJSONField(structType, name) {
			t.Errorf("notCompared names %q, which Environment no longer has", name)
		}
	}
	for _, name := range ComparedFields {
		if !hasJSONField(structType, name) {
			t.Errorf("ComparedFields names %q, which Environment no longer has", name)
		}
	}
}

func TestComparableRejectsEachComparedField(t *testing.T) {
	base := Environment{
		OS: "linux", Arch: "arm64", CPUModel: "Fixture CPU", CPUs: 8,
		Browser: "Chrome/1.2.3", Headless: true, FixtureDigest: "abc123",
		BrwVersion: "0.0.1", GoVersion: "go1.26.0", CDPProtocol: "1.3",
	}
	if ok, reason := base.Comparable(base); !ok {
		t.Fatalf("a record is not comparable with itself: %s", reason)
	}

	structType := reflect.TypeOf(Environment{})
	for _, name := range ComparedFields {
		t.Run(name, func(t *testing.T) {
			other := base
			value := reflect.ValueOf(&other).Elem()
			field, ok := fieldByJSONName(structType, name)
			if !ok {
				t.Fatalf("no field named %q", name)
			}
			target := value.FieldByIndex(field.Index)
			switch target.Kind() {
			case reflect.String:
				target.SetString(target.String() + "-changed")
			case reflect.Int:
				target.SetInt(target.Int() + 1)
			case reflect.Bool:
				target.SetBool(!target.Bool())
			default:
				t.Fatalf("field %q has kind %s, which this test cannot vary", name, target.Kind())
			}
			ok, reason := base.Comparable(other)
			if ok {
				t.Fatalf("records differing in %q were reported comparable", name)
			}
			if !strings.Contains(reason, name) {
				t.Fatalf("reason %q does not name the field %q that differs", reason, name)
			}
		})
	}
}

func TestComparableIgnoresTheExcusedFields(t *testing.T) {
	base := Environment{OS: "linux", Arch: "arm64", CPUModel: "Fixture CPU", CPUs: 8,
		Browser: "Chrome/1.2.3", Headless: true, FixtureDigest: "abc123"}
	other := base
	other.BrwVersion = "9.9.9"
	other.GoVersion = "go1.99.0"
	other.CDPProtocol = "9.9"
	other.CapturedAt = base.CapturedAt.Add(1)
	if ok, reason := base.Comparable(other); !ok {
		t.Fatalf("records differing only in excused fields were reported incomparable: %s", reason)
	}
}

func TestFingerprintNamesTheMachineAndTheBuild(t *testing.T) {
	env := Environment{
		OS: "darwin", Arch: "arm64", CPUModel: "Fixture CPU", CPUs: 16,
		Browser: "Chrome/1.2.3", BrwVersion: "0.1.0", GoVersion: "go1.26.0",
		FixtureDigest: "0123456789abcdef0123",
	}
	line := env.Fingerprint()
	for _, want := range []string{"darwin/arm64", "Fixture CPU", "x16", "Chrome/1.2.3", "brw 0.1.0", "go1.26.0", "0123456789ab"} {
		if !strings.Contains(line, want) {
			t.Errorf("fingerprint %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "0123456789abcdef0123") {
		t.Errorf("fingerprint %q prints the whole digest; it is meant to be abbreviated", line)
	}
}

func jsonName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}
	return name
}

func hasJSONField(structType reflect.Type, name string) bool {
	_, ok := fieldByJSONName(structType, name)
	return ok
}

func fieldByJSONName(structType reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if jsonName(field) == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}
