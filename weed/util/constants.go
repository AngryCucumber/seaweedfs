package util

import (
	"fmt"
)

const HttpStatusCancelled = 499

var (
	MAJOR_VERSION  = int32(3)
	MINOR_VERSION  = int32(84)
	VERSION_NUMBER = fmt.Sprintf("%d.%02d", MAJOR_VERSION, MINOR_VERSION)
	VERSION        = sizeLimit + " " + VERSION_NUMBER
	COMMIT         = ""
)

func Version() string {
	return VERSION + " " + COMMIT
}

// SizeLimit is the volume size limit of this build: "30GB", or "8000GB" when built with 5BytesOffset.
func SizeLimit() string {
	return sizeLimit
}
