package auth

import "testing"

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := CheckPassword(h, "correct horse battery"); !ok {
		t.Fatal("valid password rejected")
	}
	if ok, _ := CheckPassword(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	if ValidatePassword("short") == nil {
		t.Fatal("short password accepted")
	}
}
