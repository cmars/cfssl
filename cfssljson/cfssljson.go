// cfssljson splits out JSON with cert, csr, and key fields to separate
// files.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
)

func readFile(filespec string) ([]byte, error) {
	if filespec == "-" {
		return ioutil.ReadAll(os.Stdin)
	} else {
		return ioutil.ReadFile(filespec)
	}
}

func writeFile(filespec, contents string, perms os.FileMode) {
	err := ioutil.WriteFile(filespec, []byte(contents), perms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func main() {
	inFile := flag.String("f", "-", "JSON input")
	flag.Parse()

	var baseName string
	if flag.NArg() == 0 {
		baseName = "cert"
	} else {
		baseName = flag.Arg(0)
	}

	var input map[string]string
	fileData, err := readFile(*inFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read input: %v\n", err)
		return
	}

	err = json.Unmarshal(fileData, &input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse input: %v\n", err)
		return
	}

	if contents, ok := input["cert"]; ok {
		writeFile(baseName+".pem", contents, 0644)
	} else if contents, ok = input["certificate"]; ok {
		writeFile(baseName+".pem", contents, 0644)
	}

	if contents, ok := input["key"]; ok {
		writeFile(baseName+"-key.pem", contents, 0600)
	} else if contents, ok = input["private_key"]; ok {
		writeFile(baseName+"-key.pem", contents, 0600)
	}

	if contents, ok := input["csr"]; ok {
		writeFile(baseName+".csr", contents, 0644)
	} else if contents, ok = input["certificate_request"]; ok {
		writeFile(baseName+".csr", contents, 0644)
	}

	if contents, ok := input["bundle"]; ok {
		writeFile(baseName+"-bundle.pem", contents, 0644)
	}
}
