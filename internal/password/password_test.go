package password

import "testing"

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify("correct horse", h); !ok || err != nil {
		t.Fatalf("right password: %v %v", ok, err)
	}
	if ok, _ := Verify("wrong horse", h); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := Verify("x", "$bcrypt$nope"); err == nil {
		t.Fatal("bad hash accepted")
	}
	if h2, _ := Hash("correct horse"); h2 == h {
		t.Fatal("hash is not salted")
	}
}

func TestCheck(t *testing.T) {
	if Check("short") == nil {
		t.Fatal("short password accepted")
	}
	if Check("long enough") != nil {
		t.Fatal("good password rejected")
	}
}
