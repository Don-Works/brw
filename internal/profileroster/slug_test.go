package profileroster

import "testing"

func TestSlug(t *testing.T) {
	got, err := Slug("Bookkeeper")
	if err != nil || got != "bookkeeper" {
		t.Fatalf("Slug(Bookkeeper) = %q %v, want bookkeeper", got, err)
	}
	if _, err := Slug("2bad"); err == nil {
		t.Fatal("expected error for slug starting with a digit")
	}
	if Namespace("book-keeper") != "brw_book_keeper" {
		t.Fatalf("Namespace = %s", Namespace("book-keeper"))
	}
	if Workspace("bookkeeper") != "brw-bookkeeper" {
		t.Fatalf("Workspace = %s", Workspace("bookkeeper"))
	}
}
