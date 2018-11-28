package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Config struct {
	OutputFiles []string
	Codes       []CodeDescription
}

type CodeDescription struct {
	Name        string
	Authors     []string
	Description []string
	Build       []GeckoCode
}

type GeckoCode struct {
	Type          string
	Address       string
	TargetAddress string
	Annotation    string
	IsRecursive   bool
	SourceFile    string
	SourceFolder  string
	Value         string
}

const (
	Replace          = "replace"
	Inject           = "inject"
	ReplaceCodeBlock = "replaceCodeBlock"
	Branch           = "branch"
	BranchAndLink    = "branchAndLink"
	InjectFolder     = "injectFolder"
	ReplaceBinary    = "replaceBinary"
)

var output []string

func main() {
	defer func() {
		// Recover from panic to prevent printing stack trace
		recover()
	}()

	if len(os.Args) < 2 {
		log.Panic("Must provide a command. Try typing 'gecko build'\n")
	}

	if os.Args[1] != "build" {
		log.Panic("Currently only the build command is supported. Try typing 'gecko build'\n")
	}

	config := readConfigFile()
	if len(config.OutputFiles) < 1 {
		log.Panic("Must have at least one output file configured in the outputFiles field\n")
	}

	buildBody(config)

	for _, file := range config.OutputFiles {
		writeOutput(file)
	}
}

func readConfigFile() Config {
	contents, err := ioutil.ReadFile("codes.json")
	if err != nil {
		log.Panic("Failed to read config file codes.json\n", err)
	}

	var result Config
	err = json.Unmarshal(contents, &result)
	if err != nil {
		log.Panic(
			"Failed to get json content from config file. Check for syntax error/valid json\n",
			err,
		)
	}

	return result
}

func buildBody(config Config) {
	// go through every code and print a header and the codes that make it up
	for _, code := range config.Codes {
		headerLines := generateHeaderLines(code)
		output = append(output, headerLines...)

		codeLines := generateCodeLines(code)
		// TODO: Add description
		output = append(output, codeLines...)
		output = append(output, "")
	}
}

func generateHeaderLines(desc CodeDescription) []string {
	result := []string{}

	authorString := strings.Join(desc.Authors, ", ")
	result = append(result, fmt.Sprintf("$%s [%s]", desc.Name, authorString))

	for _, line := range desc.Description {
		result = append(result, fmt.Sprintf("*%s", line))
	}

	return result
}

func generateCodeLines(desc CodeDescription) []string {
	result := []string{}

	for _, geckoCode := range desc.Build {
		switch geckoCode.Type {
		case Replace:
			line := generateReplaceCodeLine(geckoCode.Address, geckoCode.Value)
			line = addLineAnnotation(line, geckoCode.Annotation)
			result = append(result, line)
		case Inject:
			lines := generateInjectionCodeLines(geckoCode.Address, geckoCode.SourceFile)
			lines[0] = addLineAnnotation(lines[0], geckoCode.Annotation)
			result = append(result, lines...)
		case ReplaceCodeBlock:
			lines := generateReplaceCodeBlockLines(geckoCode.Address, geckoCode.SourceFile)
			lines[0] = addLineAnnotation(lines[0], geckoCode.Annotation)
			result = append(result, lines...)
		case ReplaceBinary:
			lines := generateReplaceBinaryLines(geckoCode.Address, geckoCode.SourceFile)
			lines[0] = addLineAnnotation(lines[0], geckoCode.Annotation)
			result = append(result, lines...)
		case Branch:
			fallthrough
		case BranchAndLink:
			shouldLink := geckoCode.Type == BranchAndLink
			line := generateBranchCodeLine(geckoCode.Address, geckoCode.TargetAddress, shouldLink)
			line = addLineAnnotation(line, geckoCode.Annotation)
			result = append(result, line)
		case InjectFolder:
			lines := generateInjectionFolderLines(geckoCode.SourceFolder, geckoCode.IsRecursive)
			result = append(result, lines...)
		}
	}

	return result
}

func generateReplaceCodeLine(address, value string) string {
	// TODO: Add error if address or value is incorrect length/format
	return fmt.Sprintf("04%s %s", strings.ToUpper(address[2:]), strings.ToUpper(value))
}

func generateBranchCodeLine(address, targetAddress string, shouldLink bool) string {
	// TODO: Add error if address or value is incorrect length/format

	addressUint, err := strconv.ParseUint(address[2:], 16, 32)
	targetAddressUint, err := strconv.ParseUint(targetAddress[2:], 16, 32)
	if err != nil {
		log.Panic("Failed to parse address or target address.", err)
	}

	addressDiff := targetAddressUint - addressUint
	prefix := "48"
	if addressDiff < 0 {
		prefix = "4B"
	}

	if shouldLink {
		addressDiff += 1
	}

	// TODO: Add error if diff is going to be more than 6 characters long

	// Convert diff to hex string, and then for negative values, we
	addressDiffStr := fmt.Sprintf("%06X", addressDiff)
	addressDiffStr = addressDiffStr[len(addressDiffStr)-6:]

	return fmt.Sprintf("04%s %s%s", strings.ToUpper(address[2:]), prefix, addressDiffStr)
}

func addLineAnnotation(line, annotation string) string {
	if annotation == "" {
		return line
	}

	return fmt.Sprintf("%s #%s", line, annotation)
}

func generateInjectionFolderLines(folder string, isRecursive bool) []string {
	lines := []string{}

	contents, err := ioutil.ReadDir(folder)
	if err != nil {
		log.Panic("Failed to read directory.", err)
	}

	for _, file := range contents {
		fileName := file.Name()
		ext := filepath.Ext(fileName)
		if ext != ".asm" {
			continue
		}

		// Get full filepath for file
		filePath := filepath.Join(folder, fileName)

		file, err := os.Open(filePath)
		if err != nil {
			log.Panicf("Failed to read file at %s\n%s\n", filePath, err.Error())
		}
		defer file.Close()

		// Read first line from file to get address
		scanner := bufio.NewScanner(file)
		scanner.Scan()
		firstLine := scanner.Text()

		// Prepare injection address error
		indicateAddressError := func(errStr ...string) {
			errMsg := fmt.Sprintf(
				"File at %s needs to specify the 4 byte injection address "+
					"at the end of the first line of the file\n",
				filePath,
			)

			if len(errStr) > 0 {
				errMsg += errStr[0] + "\n"
			}

			log.Panic(errMsg)
		}

		// Get address
		lineLength := len(firstLine)
		if lineLength < 8 {
			indicateAddressError()
		}
		address := firstLine[lineLength-8:]

		_, err = hex.DecodeString(address)
		if err != nil {
			indicateAddressError(err.Error())
		}

		// Compile file and add lines
		fileLines := generateInjectionCodeLines(address, filePath)
		fileLines[0] = addLineAnnotation(fileLines[0], filePath)
		lines = append(lines, fileLines...)
	}

	if isRecursive {
		// If we are recursively searching folders, process sub-directories
		for _, file := range contents {
			if !file.IsDir() {
				continue
			}

			folderName := file.Name()
			folderPath := filepath.Join(folder, folderName)
			folderLines := generateInjectionFolderLines(folderPath, isRecursive)
			lines = append(lines, folderLines...)
		}
	}

	return lines
}

func generateInjectionCodeLines(address, file string) []string {
	// TODO: Add error if address or value is incorrect length/format
	lines := []string{}

	instructions := compile(file)
	instructionLen := len(instructions)

	if instructionLen == 0 {
		log.Panicf("Did not find any code in file: %s\n", file)
	}

	if instructionLen == 4 {
		// If instructionLen is 4, this can be a 04 code instead of C2
		instructionStr := hex.EncodeToString(instructions[0:4])
		replaceLine := generateReplaceCodeLine(address, instructionStr)
		lines = append(lines, replaceLine)

		return lines
	}

	// Fixes code to always end with 0x00000000 and have an even number of words
	if instructionLen%8 == 0 {
		instructions = append(instructions, 0x60, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	} else {
		instructions = append(instructions, 0x00, 0x00, 0x00, 0x00)
	}

	lines = append(lines, fmt.Sprintf("C2%s %08X", strings.ToUpper(address[2:]), len(instructions)/8))

	for i := 0; i < len(instructions); i += 8 {
		left := strings.ToUpper(hex.EncodeToString(instructions[i : i+4]))
		right := strings.ToUpper(hex.EncodeToString(instructions[i+4 : i+8]))
		lines = append(lines, fmt.Sprintf("%s %s", left, right))
	}

	return lines
}

func generateReplaceCodeBlockLines(address, file string) []string {
	// TODO: Add error if address or value is incorrect length/format
	lines := []string{}

	instructions := compile(file)

	// Fixes code to have an even number of words
	if len(instructions)%8 != 0 {
		instructions = append(instructions, 0x60, 0x00, 0x00, 0x00)
	}

	lines = append(lines, fmt.Sprintf("06%s %08X", strings.ToUpper(address[2:]), len(instructions)))

	for i := 0; i < len(instructions); i += 8 {
		left := strings.ToUpper(hex.EncodeToString(instructions[i : i+4]))
		right := strings.ToUpper(hex.EncodeToString(instructions[i+4 : i+8]))
		lines = append(lines, fmt.Sprintf("%s %s", left, right))
	}

	return lines
}

func generateReplaceBinaryLines(address, file string) []string {
	// TODO: Add error if address or value is incorrect length/format
	lines := []string{}

	contents, err := ioutil.ReadFile(file)
	if err != nil {
		log.Panicf("Failed to read binary file %s\n%s\n", file, err.Error())
	}

	instructions := contents

	// Fixes code to have an even number of words
	if len(instructions)%8 != 0 {
		instructions = append(instructions, 0x60, 0x00, 0x00, 0x00)
	}

	lines = append(lines, fmt.Sprintf("06%s %08X", strings.ToUpper(address[2:]), len(instructions)))

	for i := 0; i < len(instructions); i += 8 {
		left := strings.ToUpper(hex.EncodeToString(instructions[i : i+4]))
		right := strings.ToUpper(hex.EncodeToString(instructions[i+4 : i+8]))
		lines = append(lines, fmt.Sprintf("%s %s", left, right))
	}

	return lines
}

func compile(file string) []byte {
	defer os.Remove("a.out")

	// First we are gonna load all the data from file and write it into temp file
	// Technically this shouldn't be necessary but for some reason if the last line
	// or the asm file has one of more spaces at the end and no new line, the last
	// instruction is ignored and not compiled
	asmContents, err := ioutil.ReadFile(file)
	if err != nil {
		log.Panicf("Failed to read asm file: %s\n%s\n", file, err.Error())
	}

	// Explicitly add a new line at the end of the file, which should prevent line skip
	asmContents = append(asmContents, []byte("\r\n")...)
	err = ioutil.WriteFile("asm-to-compile.asm", asmContents, 0644)
	if err != nil {
		log.Panicf("Failed to write temporary asm file\n%s\n", err.Error())
	}
	defer os.Remove("asm-to-compile.asm")

	if runtime.GOOS == "windows" {
		cmd := exec.Command("powerpc-gekko-as.exe", "-a32", "-mbig", "-mregnames", "-mgekko", "asm-to-compile.asm")
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Failed to compile file: %s\n", file)
			fmt.Printf("%s", output)
			os.Exit(1)
		}
		contents, err := ioutil.ReadFile("a.out")
		if err != nil {
			log.Panicf("Failed to read compiled file %s\n%s\n", file, err.Error())
		}

		// I don't understand how this works (?)
		codeEndIndex := bytes.Index(contents, []byte{0x00, 0x2E, 0x73, 0x79, 0x6D, 0x74, 0x61, 0x62})
		return contents[52:codeEndIndex]
	}

	// Just pray that powerpc-eabi-{as,objcopy} are in the user's $PATH, lol
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		cmd := exec.Command("powerpc-eabi-as", "-a32", "-mbig", "-mregnames", "asm-to-compile.asm")
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Failed to compile file: %s\n", file)
			fmt.Printf("%s", output)
			os.Exit(1)
		}
		cmd = exec.Command("powerpc-eabi-objcopy", "-O", "binary", "a.out", "a.out")
		output, err = cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Failed to pull out .text section: %s\n", file)
			fmt.Printf("%s", output)
			os.Exit(1)
		}
		contents, err := ioutil.ReadFile("a.out")
		if err != nil {
			log.Panicf("Failed to read compiled file %s\n%s\n", file, err.Error())
		}
		return contents
	}

	log.Panicf("Platform unsupported\n")
	os.Exit(1)
	return nil
}

func writeOutput(outputFile string) {
	fmt.Printf("Writing to %s...\n", outputFile)
	ext := filepath.Ext(outputFile)
	switch ext {
	case ".gct":
		writeGctOutput(outputFile)
	default:
		writeTextOutput(outputFile)
	}

	fmt.Printf("Successfuly wrote codes to %s\n", outputFile)
}

func writeTextOutput(outputFile string) {
	fullText := strings.Join(output, "\n")
	ioutil.WriteFile(outputFile, []byte(fullText), 0644)
}

func writeGctOutput(outputFile string) {
	gctBytes := []byte{0x00, 0xD0, 0xC0, 0xDE, 0x00, 0xD0, 0xC0, 0xDE}

	for _, line := range output {
		if len(line) < 17 {
			// lines with less than 17 characters cannot be code lines
			continue
		}

		lineBytes, err := hex.DecodeString(line[0:8] + line[9:17])
		if err != nil {
			// If parse fails that likely means this is a header or something
			continue
		}

		gctBytes = append(gctBytes, lineBytes...)
	}

	gctBytes = append(gctBytes, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	ioutil.WriteFile(outputFile, gctBytes, 0644)
}
