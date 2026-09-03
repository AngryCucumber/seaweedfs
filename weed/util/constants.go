package util

import (
	"fmt"
)

const HttpStatusCancelled = 499

var (
	MAJOR_VERSION  = int32(2)
	MINOR_VERSION  = int32(0)
	PATCH_VERSION  = int32(5)
	VERSION_NUMBER = fmt.Sprintf("%d.%d.%d", MAJOR_VERSION, MINOR_VERSION, PATCH_VERSION)
	VERSION        = sizeLimit + " " + VERSION_NUMBER
	COMMIT         = ""
)

func Version() string {
	return VERSION + " " + COMMIT
}
