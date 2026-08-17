package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	f, err := os.Open(`C:\Users\Gerson\.gemini\antigravity-cli\brain\20c4ce35-0c3c-4c0d-945f-c856ee591810\.system_generated\logs\transcript.jsonl`)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "20260812-3") || strings.Contains(line, "docker build") {
			if len(line) > 300 {
				line = line[:300]
			}
			fmt.Println(line)
		}
	}
}
