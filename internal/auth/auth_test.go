package auth

import (
	"log"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMakeJwt(t *testing.T) {

	id1 := uuid.New()
	token, err := MakeJWT(id1, "foobar", time.Duration(1*time.Minute))
	if err != nil {
		t.Fatalf("MakeJWT failed: %v", err)
	}

	id2, err := ValidateJWT(token, "foobar")
	if err != nil {
		t.Fatalf("ValidateJWT failed: %v", err)
	}

	if id1 != id2 {
		t.Fatalf("ids not equal, %s != %s", id1, id2)
	}
}

func TestExpiredToken(t *testing.T) {
	id1 := uuid.New()
	token, err := MakeJWT(id1, "foobar", time.Duration(1*time.Second))
	if err != nil {
		t.Fatalf("MakeJWT failed: %v", err)
	}

	time.Sleep(2 * time.Second)

	_, err = ValidateJWT(token, "foobar")
	if err == nil {
		t.Fatalf("unexpected success")
	}
	log.Printf("err reason: %v", err)
}
