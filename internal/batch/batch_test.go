package batch

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		n    int
		want []int
	}{
		{0, nil},
		{1, []int{1}},
		{100, []int{100}},
		{150, []int{150}},
		{151, []int{100, 51}},
		{250, []int{100, 150}},
		{251, []int{100, 100, 51}},
		{400, []int{100, 100, 100, 100}},
	}
	for _, tc := range tests {
		ids := make([]uint64, tc.n)
		for i := range ids {
			ids[i] = uint64(i + 1)
		}
		got := Split(ids, 100, 150)
		var lens []int
		var flat []uint64
		for _, b := range got {
			lens = append(lens, len(b))
			flat = append(flat, b...)
		}
		if !reflect.DeepEqual(lens, tc.want) {
			t.Errorf("n=%d: lens %v want %v", tc.n, lens, tc.want)
		}
		if len(flat) != len(ids) || (len(ids) > 0 && !reflect.DeepEqual(flat, ids)) {
			t.Errorf("n=%d: order/content mismatch", tc.n)
		}
	}
}
