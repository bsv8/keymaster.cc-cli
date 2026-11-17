package main

import (
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

func printQR(value string) {
	code, err := qrcode.New(value, qrcode.Low)
	if err != nil {
		return
	}
	bitmap := code.Bitmap()
	for _, row := range bitmap {
		for _, v := range row {
			if v {
				fmt.Print("██")
			} else {
				fmt.Print("  ")
			}
		}
		fmt.Println()
	}
}
