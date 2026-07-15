package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/xml"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"syscall/js"
)

type Record struct {
	Date       string
	PostTime   string // 카카오톡 게시시간(HH:MM)
	Driver     string
	Vehicle    string
	Start      *int
	End        *int
	Distance   *int
	Range      *int
	Note       string
	Correction string // 자동 보정 내역(예: 연속성 +1km)
}

type Judged struct {
	Record
	PrevRange          *int
	RangeDelta         *int // 전회 주행가능거리 - 현재 주행가능거리(원값, 음수 가능)
	RangeDecrease      *int // 판정에 사용하는 감소량: MAX(0, RangeDelta)
	SuspicionIndex     *float64
	VehicleStableIndex *float64
	VehicleBaseIndex   *float64 // 최근 정상 N건 기준 의심지수
	RawCorrection      *float64
	VehicleCorrection  float64
	DynamicOffset      *float64
	AllowedDecrease    *float64
	Judge              string
}

var people = []string{"5급 우재익", "중사 이상국", "6급 박효재", "7급 김지영", "6급 김봉조", "6급 장현익", "6급 안성준", "7급 하예주"}
var vehicles = []string{"아반떼", "모닝", "기타"}

const defaultMultiplier = 1.35
const defaultMarginKm = 10.0
const defaultOverdriveKm = 40
const defaultBaseSuspicionIndex = 2.0
const defaultCorrectionWidth = 0.4
const defaultCorrectionSensitivity = 4.5
const defaultOffsetBaseDistance = 10.0
const defaultMinBaseRecords = 5
const defaultRecentBaseRecords = 10
const shortTripMaxDistance = 5
const shortTripNormalDecrease = 12
const shortTripCautionDecrease = 25
const correctionLearningMinDistance = 4

type MonthFilter struct {
	Year    int
	Month   int
	Enabled bool
}

type VehicleLogInfo struct {
	UnitDepartment string
	AvanteNumber   string
	MorningNumber  string
}

type Settings struct {
	Multiplier            float64
	MarginKm              float64
	OverdriveKm           int
	BaseSuspicionIndex    float64
	CorrectionWidth       float64
	CorrectionSensitivity float64
	OffsetBaseDistance    float64
	MinBaseRecords        int
	RecentBaseRecords     int
}

func cliMain() {
	if len(os.Args) < 2 {
		fmt.Println("차량 운행 관리 프로그램 v3.93")
		fmt.Println("사용법: 차량운행관리.exe 카카오톡내보내기.txt")
		fmt.Println("txt 파일을 exe 위로 드래그해도 됩니다.")
		pause()
		return
	}
	in := os.Args[1]
	text, err := readText(in)
	if err != nil {
		fmt.Println("파일 읽기 오류:", err)
		pause()
		return
	}
	allRecords := parseRecords(text)
	filter := askMonthFilter(allRecords)
	settings := askSettings()
	logInfo := askVehicleLogInfo()
	judged := judgeRecordsFiltered(allRecords, filter, settings)
	out := outputPath(in, filter)
	if err := createXLSX(out, judged, filter, settings, logInfo); err != nil {
		fmt.Println("엑셀 생성 오류:", err)
		pause()
		return
	}
	fmt.Println("완료:", out)
	if filter.Enabled {
		fmt.Printf("대상월: %04d년 %d월\n", filter.Year, filter.Month)
	} else {
		fmt.Println("대상월: 전체")
	}
	fmt.Printf("최종 운행기록: %d건\n", len(judged))
	fmt.Printf("설정값: 배율 %.2f / 기본여유 %.0fkm / 과다운전 기준 %dkm / 기본의심지수 %.2f / 보정폭 %.2f / 민감도 %.2f\n", settings.Multiplier, settings.MarginKm, settings.OverdriveKm, settings.BaseSuspicionIndex, settings.CorrectionWidth, settings.CorrectionSensitivity)
	pause()
}
func pause() {
	fmt.Println("창을 닫으려면 Enter를 누르세요...")
	bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func askVehicleLogInfo() VehicleLogInfo {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("차량운행일지 부대명/부서명을 입력하세요. Enter=기술관리실: ")
	unit, _ := reader.ReadString('\n')
	unit = strings.TrimSpace(unit)
	if unit == "" {
		unit = "기술관리실"
	}

	fmt.Print("아반떼 차량번호를 입력하세요. Enter=175허5481: ")
	avante, _ := reader.ReadString('\n')
	avante = strings.TrimSpace(avante)
	if avante == "" {
		avante = "175허5481"
	}

	fmt.Print("모닝 차량번호를 입력하세요. Enter=175허5506: ")
	morning, _ := reader.ReadString('\n')
	morning = strings.TrimSpace(morning)
	if morning == "" {
		morning = "175허5506"
	}
	return VehicleLogInfo{UnitDepartment: unit, AvanteNumber: avante, MorningNumber: morning}
}

func askSettings() Settings {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("주행가능거리 과다 감소 배율을 입력하세요. Enter=%.2f: ", defaultMultiplier)
	multLine, _ := reader.ReadString('\n')
	multLine = strings.TrimSpace(multLine)
	multiplier := defaultMultiplier
	if multLine != "" {
		if v, err := strconv.ParseFloat(multLine, 64); err == nil && v > 0 {
			multiplier = v
		} else {
			fmt.Println("배율 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("기본여유(km)를 입력하세요. Enter=%.0f: ", defaultMarginKm)
	marginLine, _ := reader.ReadString('\n')
	marginLine = strings.TrimSpace(marginLine)
	margin := defaultMarginKm
	if marginLine != "" {
		if v, err := strconv.ParseFloat(marginLine, 64); err == nil && v >= 0 {
			margin = v
		} else {
			fmt.Println("기본여유 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("과다운전 기준(km)을 입력하세요. Enter=%d: ", defaultOverdriveKm)
	overLine, _ := reader.ReadString('\n')
	overLine = strings.TrimSpace(overLine)
	over := defaultOverdriveKm
	if overLine != "" {
		if v, err := strconv.Atoi(overLine); err == nil && v > 0 {
			over = v
		} else {
			fmt.Println("과다운전 기준 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("기본 의심지수를 입력하세요. Enter=%.2f: ", defaultBaseSuspicionIndex)
	baseLine, _ := reader.ReadString('\n')
	baseLine = strings.TrimSpace(baseLine)
	baseSuspicion := defaultBaseSuspicionIndex
	if baseLine != "" {
		if v, err := strconv.ParseFloat(baseLine, 64); err == nil && v > 0 {
			baseSuspicion = v
		} else {
			fmt.Println("기본 의심지수 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("보정 최대폭을 입력하세요. Enter=%.2f: ", defaultCorrectionWidth)
	widthLine, _ := reader.ReadString('\n')
	widthLine = strings.TrimSpace(widthLine)
	width := defaultCorrectionWidth
	if widthLine != "" {
		if v, err := strconv.ParseFloat(widthLine, 64); err == nil && v >= 0 {
			width = v
		} else {
			fmt.Println("보정 최대폭 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("보정 민감도를 입력하세요. Enter=%.2f: ", defaultCorrectionSensitivity)
	sensLine, _ := reader.ReadString('\n')
	sensLine = strings.TrimSpace(sensLine)
	sens := defaultCorrectionSensitivity
	if sensLine != "" {
		if v, err := strconv.ParseFloat(sensLine, 64); err == nil && v >= 0 {
			sens = v
		} else {
			fmt.Println("보정 민감도 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("오프셋 기준거리(km)를 입력하세요. Enter=%.0f: ", defaultOffsetBaseDistance)
	offsetDistLine, _ := reader.ReadString('\n')
	offsetDistLine = strings.TrimSpace(offsetDistLine)
	offsetBaseDistance := defaultOffsetBaseDistance
	if offsetDistLine != "" {
		if v, err := strconv.ParseFloat(offsetDistLine, 64); err == nil && v > 0 {
			offsetBaseDistance = v
		} else {
			fmt.Println("오프셋 기준거리 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("보정치 산정 최소 정상 기록 수를 입력하세요. Enter=%d: ", defaultMinBaseRecords)
	minLine, _ := reader.ReadString('\n')
	minLine = strings.TrimSpace(minLine)
	minBase := defaultMinBaseRecords
	if minLine != "" {
		if v, err := strconv.Atoi(minLine); err == nil && v > 0 {
			minBase = v
		} else {
			fmt.Println("최소 정상 기록 수 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	fmt.Printf("차량보정치 산정 최근 정상 기록 수를 입력하세요. Enter=%d: ", defaultRecentBaseRecords)
	recentLine, _ := reader.ReadString('\n')
	recentLine = strings.TrimSpace(recentLine)
	recentBase := defaultRecentBaseRecords
	if recentLine != "" {
		if v, err := strconv.Atoi(recentLine); err == nil && v > 0 {
			recentBase = v
		} else {
			fmt.Println("최근 정상 기록 수 입력이 올바르지 않아 기본값을 사용합니다.")
		}
	}

	return Settings{Multiplier: multiplier, MarginKm: margin, OverdriveKm: over, BaseSuspicionIndex: baseSuspicion, CorrectionWidth: width, CorrectionSensitivity: sens, OffsetBaseDistance: offsetBaseDistance, MinBaseRecords: minBase, RecentBaseRecords: recentBase}
}

func askMonthFilter(records []Record) MonthFilter {
	reader := bufio.NewReader(os.Stdin)
	defYear, defMonth := latestYearMonth(records)
	if defYear > 0 {
		fmt.Printf("대상 연도를 입력하세요. Enter=%d: ", defYear)
	} else {
		fmt.Print("대상 연도를 입력하세요. Enter=전체: ")
	}
	yline, _ := reader.ReadString('\n')
	yline = strings.TrimSpace(yline)
	if yline == "" {
		if defYear == 0 {
			return MonthFilter{}
		}
	}
	year := defYear
	if yline != "" {
		if v, err := strconv.Atoi(yline); err == nil {
			year = v
		} else {
			fmt.Println("연도 입력이 올바르지 않아 전체로 생성합니다.")
			return MonthFilter{}
		}
	}

	if defMonth > 0 {
		fmt.Printf("대상 월을 입력하세요. Enter=%d, 0=전체: ", defMonth)
	} else {
		fmt.Print("대상 월을 입력하세요. 0 또는 Enter=전체: ")
	}
	mline, _ := reader.ReadString('\n')
	mline = strings.TrimSpace(mline)
	if mline == "" {
		if defMonth == 0 {
			return MonthFilter{}
		}
		return MonthFilter{Year: year, Month: defMonth, Enabled: true}
	}
	month, err := strconv.Atoi(mline)
	if err != nil || month < 0 || month > 12 {
		fmt.Println("월 입력이 올바르지 않아 전체로 생성합니다.")
		return MonthFilter{}
	}
	if month == 0 {
		return MonthFilter{}
	}
	return MonthFilter{Year: year, Month: month, Enabled: true}
}

func latestYearMonth(records []Record) (int, int) {
	y, m := 0, 0
	for _, r := range records {
		yy, mm, ok := yearMonth(r.Date)
		if !ok {
			continue
		}
		if yy > y || (yy == y && mm > m) {
			y = yy
			m = mm
		}
	}
	return y, m
}

func yearMonth(date string) (int, int, bool) {
	parts := strings.Split(date, "-")
	if len(parts) < 2 {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return y, m, true
}

func inFilter(r Record, f MonthFilter) bool {
	if !f.Enabled {
		return true
	}
	y, m, ok := yearMonth(r.Date)
	return ok && y == f.Year && m == f.Month
}

func outputPath(in string, f MonthFilter) string {
	base := strings.TrimSuffix(in, filepath.Ext(in))
	if f.Enabled {
		return fmt.Sprintf("%s_%04d년_%02d월_차량운행보고서.xlsx", base, f.Year, f.Month)
	}
	return base + "_차량운행보고서.xlsx"
}

func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// assume UTF-8 / CP949 일부 문자는 깨질 수 있으나 카톡 내보내기 UTF-8 기준
	return string(bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})), nil
}

func normDate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "’", "")
	re4 := regexp.MustCompile(`(20\d{2})[-./]\s*(\d{1,2})[-./]\s*(\d{1,2})`)
	if m := re4.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return fmt.Sprintf("%04d-%02d-%02d", y, mo, d)
	}
	re2 := regexp.MustCompile(`(\d{2})\s*[./-]\s*(\d{1,2})\s*[./-]\s*(\d{1,2})`)
	if m := re2.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return fmt.Sprintf("%04d-%02d-%02d", 2000+y, mo, d)
	}
	return s
}
func normalizeDriver(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(미기재)"
	}
	s = strings.ReplaceAll(s, "/", " ")
	s = strings.ReplaceAll(s, "(", " ")
	s = strings.ReplaceAll(s, ")", " ")
	for _, p := range people {
		name := lastName(p)
		if strings.Contains(s, name) {
			return name
		}
	}
	re := regexp.MustCompile(`^(?:\d+급|중사|상사|원사|하사|소위|중위|대위|소령|중령|대령)\s+`)
	s = re.ReplaceAllString(s, "")
	fields := strings.Fields(s)
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return s
}

func parseInt(s string) *int {
	re := regexp.MustCompile(`[-+]?\d[\d,]*`)
	m := re.FindString(s)
	if m == "" {
		return nil
	}
	v, err := strconv.Atoi(strings.ReplaceAll(m, ",", ""))
	if err != nil {
		return nil
	}
	return &v
}
func field(block string, labels ...string) string {
	lines := strings.Split(block, "\n")
	for _, line := range lines {
		for _, lab := range labels {
			lineTrim := strings.TrimSpace(line)
			if strings.HasPrefix(lineTrim, lab) {
				parts := strings.SplitN(lineTrim, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
				parts = strings.SplitN(lineTrim, "：", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
	}
	return ""
}

func kakaoSeparatorDate(line string) string {
	re := regexp.MustCompile(`^-+\s*(\d{4})년\s*(\d{1,2})월\s*(\d{1,2})일`)
	if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return fmt.Sprintf("%04d-%02d-%02d", y, mo, d)
	}
	return ""
}

func normalizePostTime(s string) string {
	s = strings.TrimSpace(s)
	re := regexp.MustCompile(`^(오전|오후)\s+(\d{1,2}):(\d{2})$`)
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	hour, err1 := strconv.Atoi(m[2])
	minute, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || hour < 1 || hour > 12 || minute < 0 || minute > 59 {
		return ""
	}
	if m[1] == "오전" {
		if hour == 12 {
			hour = 0
		}
	} else if hour != 12 {
		hour += 12
	}
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

func blocks(text string) []string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	header := regexp.MustCompile(`^\[[^\]]+\]\s+\[((?:오전|오후)\s+\d{1,2}:\d{2})\]`)
	var out []string
	var cur []string
	currentDate := ""
	flush := func() {
		if len(cur) > 0 {
			msg := strings.Join(cur, "\n")
			if strings.Contains(msg, "[차량 운행 현황]") {
				out = append(out, msg)
			}
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if sepDate := kakaoSeparatorDate(line); sepDate != "" {
			flush()
			cur = nil
			currentDate = sepDate
			continue
		}
		if m := header.FindStringSubmatch(line); m != nil {
			flush()
			postTime := normalizePostTime(m[1])
			idx := strings.Index(line, "] ")
			if idx >= 0 { // 닉네임과 시간 헤더를 제거하고 메시지 본문만 남긴다.
				rest := line[idx+2:]
				idx2 := strings.Index(rest, "]")
				if idx2 >= 0 {
					line = strings.TrimSpace(rest[idx2+1:])
				}
			}
			cur = []string{line}
			if postTime != "" {
				cur = append(cur, "카톡게시시간: "+postTime)
			}
			if currentDate != "" {
				cur = append(cur, "카톡기준일자: "+currentDate)
			}
		} else {
			if len(cur) > 0 {
				cur = append(cur, line)
			}
		}
	}
	flush()
	if len(out) == 0 && strings.Contains(text, "[차량 운행 현황]") {
		parts := strings.Split(text, "[차량 운행 현황]")
		for _, p := range parts[1:] {
			out = append(out, "[차량 운행 현황]"+p)
		}
	}
	return out
}

func recordFromBlock(b string) *Record {
	start := parseInt(field(b, "운행 시작 누적거리(km)", "운행시작누적거리(km)", "운행 시작 누적거리", "시작거리"))
	end := parseInt(field(b, "운행 종료 누적거리(km)", "운행종료누적거리(km)", "운행 종료 누적거리", "종료거리"))
	if start == nil && end == nil {
		return nil
	}
	vehicle := field(b, "운전차량", "차량")
	if strings.Contains(vehicle, ",") || strings.Contains(vehicle, "，") {
		return nil
	}
	date := normDate(field(b, "카톡기준일자"))
	if date == "" {
		date = normDate(field(b, "일자"))
	}
	postTime := field(b, "카톡게시시간")
	driver := normalizeDriver(field(b, "운전자"))
	rng := parseInt(field(b, "주행 가능 거리(km)", "주행가능거리(km)", "주행 가능 거리", "주행가능거리"))
	note := field(b, "비고", "원문비고")
	var dist *int
	if start != nil && end != nil {
		d := *end - *start
		dist = &d
	}
	if vehicle == "" {
		vehicle = "(미기재)"
	}
	return &Record{Date: date, PostTime: postTime, Driver: driver, Vehicle: vehicle, Start: start, End: end, Distance: dist, Range: rng, Note: note}
}
func score(r Record) int {
	n := 0
	vals := []string{r.Date, r.Driver, r.Vehicle, r.Note}
	for _, v := range vals {
		if v != "" && v != "(미기재)" {
			n++
		}
	}
	if r.Start != nil {
		n += 2
	}
	if r.End != nil {
		n += 3
	}
	if r.Distance != nil {
		n += 2
	}
	if r.Range != nil {
		n++
	}
	return n
}
func mergeRecord(a, b Record) Record {
	// 같은 운행 후보 병합: 날짜+운전자+차량+시작거리 기준
	if a.Date == "" {
		a.Date = b.Date
	}
	if b.PostTime != "" {
		a.PostTime = b.PostTime
	}
	if a.Driver == "" || a.Driver == "(미기재)" {
		a.Driver = b.Driver
	}
	if a.Vehicle == "" || a.Vehicle == "(미기재)" {
		a.Vehicle = b.Vehicle
	}
	if a.Start == nil {
		a.Start = b.Start
	}
	if b.End != nil {
		a.End = b.End
	}
	if b.Range != nil {
		a.Range = b.Range
	}
	if strings.TrimSpace(b.Note) != "" && !strings.Contains(a.Note, b.Note) {
		if strings.TrimSpace(a.Note) == "" {
			a.Note = b.Note
		} else {
			a.Note = a.Note + " / " + b.Note
		}
	}
	if a.Start != nil && a.End != nil {
		d := *a.End - *a.Start
		a.Distance = &d
	} else {
		a.Distance = nil
	}
	// 혹시 b가 더 완전한데 위 병합으로 해결되지 않는 경우 대비
	if score(b) > score(a) && a.End == nil {
		a = b
	}
	return a
}
func mergeKey(r Record) string {
	driver := r.Driver
	if driver == "" {
		driver = "(미기재)"
	}
	return fmt.Sprintf("%s|%s|%s|%v", r.Date, driver, r.Vehicle, ptrStr(r.Start))
}

func exactDuplicateKey(r Record) string {
	// 같은 운행정보를 비고 보완 등의 이유로 며칠 뒤 다시 등록한 경우도 1건으로 병합한다.
	// 날짜는 재작성한 날짜로 달라질 수 있으므로 키에서 제외한다.
	// 누적거리와 주행가능거리가 모두 같은 기록은 별개의 운행일 수 없으므로 동일 운행으로 본다.
	return fmt.Sprintf("%s|%v|%v|%v", r.Vehicle, ptrStr(r.Start), ptrStr(r.End), ptrStr(r.Range))
}

func mergeNotes(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" || strings.Contains(a, b) {
		return a
	}
	if strings.Contains(b, a) {
		return b
	}
	return a + " / " + b
}

func preferRecord(old Record, newer Record) Record {
	// 더 완전한 글을 우선하되, 같은 운행을 며칠 뒤 재작성한 경우 실제 운행일인 이른 날짜를 보존한다.
	earliestDate := old.Date
	if earliestDate == "" || (newer.Date != "" && newer.Date < earliestDate) {
		earliestDate = newer.Date
	}

	merged := old
	if score(newer) >= score(old) {
		merged = newer
		if (merged.Driver == "" || merged.Driver == "(미기재)") && old.Driver != "" && old.Driver != "(미기재)" {
			merged.Driver = old.Driver
		}
	} else if (merged.Driver == "" || merged.Driver == "(미기재)") && newer.Driver != "" && newer.Driver != "(미기재)" {
		merged.Driver = newer.Driver
	}
	merged.Date = earliestDate
	if merged.PostTime == "" {
		if newer.PostTime != "" {
			merged.PostTime = newer.PostTime
		} else {
			merged.PostTime = old.PostTime
		}
	}
	merged.Note = mergeNotes(old.Note, newer.Note)
	return merged
}

func parseRecords(text string) []Record {
	var recs []Record
	for _, b := range blocks(text) {
		if r := recordFromBlock(b); r != nil {
			recs = append(recs, *r)
		}
	}

	// 1차: 출발 등록 + 종료 등록 병합
	// 기준: 날짜 + 운전자(정규화 후) + 차량 + 시작거리
	best := map[string]Record{}
	order := []string{}
	for _, r := range recs {
		key := mergeKey(r)
		if old, ok := best[key]; ok {
			best[key] = mergeRecord(old, r)
		} else {
			best[key] = r
			order = append(order, key)
		}
	}

	merged := make([]Record, 0, len(best))
	for _, key := range order {
		merged = append(merged, best[key])
	}

	// 2차: 같은 운행 실적을 비고 보완 등의 이유로 다시 등록한 경우 제거
	// 기준: 차량 + 시작거리 + 종료거리 + 주행가능거리(날짜가 달라도 병합)
	// 예: 운전자 (미기재) 26209→26237 / 운전자 김봉조 26209→26237 → 김봉조 1건만 반영
	exactBest := map[string]Record{}
	exactOrder := []string{}
	for _, r := range merged {
		key := exactDuplicateKey(r)
		if old, ok := exactBest[key]; ok {
			exactBest[key] = preferRecord(old, r)
		} else {
			exactBest[key] = r
			exactOrder = append(exactOrder, key)
		}
	}

	out := make([]Record, 0, len(exactBest))
	for _, key := range exactOrder {
		out = append(out, exactBest[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		if out[i].Vehicle != out[j].Vehicle {
			return out[i].Vehicle < out[j].Vehicle
		}
		return val(out[i].Start, 1<<30) < val(out[j].Start, 1<<30)
	})
	return out
}
func ptrStr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}
func val(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}

func previousMonth(y int, m int) (int, int) {
	if m <= 1 {
		return y - 1, 12
	}
	return y, m - 1
}

func sameYearMonth(date string, y int, m int) bool {
	yy, mm, ok := yearMonth(date)
	return ok && yy == y && mm == m
}

func medianFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func calcVehicleCorrection(stableIndex float64, recentIndex float64, settings Settings) (float64, float64) {
	if stableIndex <= 0 || recentIndex <= 0 {
		return 1.0, 1.0
	}
	// 원보정치 = 차량 장기 정상 기준 ÷ 최근 정상 N건 기준
	// 1.00을 중심으로 최근 정상 패턴이 장기 기준보다 좋아졌는지/나빠졌는지 표현한다.
	raw := stableIndex / recentIndex
	corr := 1.0 + settings.CorrectionWidth*(2.0/math.Pi)*math.Atan(settings.CorrectionSensitivity*(raw-1.0))
	if corr <= 0 {
		corr = 1.0
	}
	return raw, corr
}

func dynamicOffset(distance int, settings Settings) float64 {
	if distance <= 0 || settings.OffsetBaseDistance <= 0 {
		return 0
	}
	ratio := float64(distance) / settings.OffsetBaseDistance
	if ratio > 1 {
		ratio = 1
	}
	return settings.MarginKm * ratio
}

func decreaseJudgeByDistance(distance int, decrease int, allowed float64) string {
	// 1~5km 단거리 운행은 냉간시동·짧은 이동·계기판 재계산 영향으로
	// 비율식이 과민하게 반응하므로 절대 감소량 기준을 우선 적용한다.
	if distance <= shortTripMaxDistance {
		if decrease <= shortTripNormalDecrease {
			return "정상"
		}
		if decrease <= shortTripCautionDecrease {
			return "단거리 과다감소 주의"
		}
		return "단거리 주행가능거리 과다 감소"
	}

	if float64(decrease) > allowed*1.2 {
		return "운행거리 대비 주행가능거리 과다 감소"
	}
	if float64(decrease) > allowed {
		return "운행거리 대비 주행가능거리 감소 주의"
	}
	return "정상"
}

func validBasicRecord(r Record) bool {
	return r.Date != "" && r.Driver != "" && r.Driver != "(미기재)" && r.Vehicle != "" && r.Start != nil && r.End != nil && r.Distance != nil && r.Range != nil && *r.End >= *r.Start && *r.Distance > 0
}

func preliminaryNormalForBase(r Record, prev Record, settings Settings) (float64, bool) {
	if !validBasicRecord(r) || prev.End == nil || prev.Range == nil || r.Range == nil || r.Start == nil {
		return 0, false
	}
	if *r.Start != *prev.End {
		return 0, false
	}
	if r.Distance != nil && *r.Distance >= settings.OverdriveKm {
		return 0, false
	}
	rawDelta := *prev.Range - *r.Range
	decrease := rawDelta
	if decrease < 0 {
		decrease = 0
	}
	// 가능거리 증가/무감소 기록은 과다 유류소모 기준값 산정에는 사용하지 않는다.
	if decrease <= 0 || r.Distance == nil || *r.Distance <= 0 {
		return 0, false
	}
	// 1~3km 정상 기록은 판정에는 필요하지만, 의심지수 분모가 작아 차량 기준값을 오염시킬 수 있어 기준 산정에서 제외한다.
	if *r.Distance < correctionLearningMinDistance {
		return 0, false
	}
	suspicion := float64(decrease) / float64(*r.Distance)
	allowed := float64(*r.Distance)*settings.Multiplier + dynamicOffset(*r.Distance, settings)
	if decreaseJudgeByDistance(*r.Distance, decrease, allowed) != "정상" {
		return suspicion, false
	}
	return suspicion, true
}

func vehicleBaseIndexes(records []Record, filter MonthFilter, settings Settings) map[string]float64 {
	result := map[string]float64{}
	if !filter.Enabled {
		return result
	}
	py, pm := previousMonth(filter.Year, filter.Month)
	groups := map[string][]int{}
	for i, r := range records {
		groups[r.Vehicle] = append(groups[r.Vehicle], i)
	}
	for vehicle, idxs := range groups {
		sort.SliceStable(idxs, func(a, b int) bool {
			ra, rb := records[idxs[a]], records[idxs[b]]
			if ra.Date != rb.Date {
				return ra.Date < rb.Date
			}
			return val(ra.Start, 1<<30) < val(rb.Start, 1<<30)
		})
		vals := []float64{}
		prev := -1
		for _, idx := range idxs {
			r := records[idx]
			if prev >= 0 && sameYearMonth(r.Date, py, pm) {
				if suspicion, ok := preliminaryNormalForBase(r, records[prev], settings); ok {
					vals = append(vals, suspicion)
				}
			}
			prev = idx
		}
		if len(vals) >= settings.MinBaseRecords {
			result[vehicle] = medianFloat(vals)
		}
	}
	return result
}

func recentFloatValues(vals []float64, n int) []float64 {
	if n <= 0 || len(vals) == 0 {
		return nil
	}
	if len(vals) <= n {
		return append([]float64(nil), vals...)
	}
	return append([]float64(nil), vals[len(vals)-n:]...)
}

func isNormalEquivalent(judge string) bool {
	return judge == "정상" || judge == "연속성 보정" || judge == "단거리 과다감소 주의"
}

func shouldUseForCorrection(j Judged) bool {
	if !isNormalEquivalent(j.Judge) || j.SuspicionIndex == nil || j.RangeDecrease == nil || j.Distance == nil {
		return false
	}
	if *j.Distance < correctionLearningMinDistance {
		return false
	}
	if *j.RangeDecrease <= 0 || *j.SuspicionIndex <= 0 {
		return false
	}
	return true
}

func appendCorrection(existing string, added string) string {
	if strings.TrimSpace(existing) == "" {
		return added
	}
	return existing + " / " + added
}

func applyContinuityCorrections(records []Record) []Record {
	corrected := append([]Record(nil), records...)
	groups := map[string][]int{}
	for i, r := range corrected {
		groups[r.Vehicle] = append(groups[r.Vehicle], i)
	}
	for _, idxs := range groups {
		sort.SliceStable(idxs, func(a, b int) bool {
			ra, rb := corrected[idxs[a]], corrected[idxs[b]]
			if ra.Date != rb.Date {
				return ra.Date < rb.Date
			}
			return val(ra.Start, 1<<30) < val(rb.Start, 1<<30)
		})

		// 시작거리 복사 실수 보정:
		// 현재 시작거리가 전회 시작거리와 같고, 현재 종료거리가 다음 운행 시작거리와 이어지며,
		// 주행가능거리도 전회와 달라 실제 운행이 확인되는 경우 현재 시작거리를 전회 종료거리로 맞춘다.
		for pos := 1; pos+1 < len(idxs); pos++ {
			prev := &corrected[idxs[pos-1]]
			cur := &corrected[idxs[pos]]
			next := corrected[idxs[pos+1]]
			if prev.Start == nil || prev.End == nil || prev.Range == nil || cur.Start == nil || cur.End == nil || cur.Range == nil || next.Start == nil {
				continue
			}
			if *cur.Start != *prev.Start || *cur.End <= *prev.End || *next.Start != *cur.End || *cur.Range == *prev.Range {
				continue
			}
			oldStart := *cur.Start
			newStart := *prev.End
			if newStart >= *cur.End {
				continue
			}
			cur.Start = intPtr(newStart)
			cur.Distance = intPtr(*cur.End - newStart)
			cur.Correction = appendCorrection(cur.Correction, fmt.Sprintf("연속성 보정: 시작거리 %dkm → %dkm", oldStart, newStart))
		}

		// 1km 미기록 보정: 현재 시작거리가 전회 종료거리보다 정확히 1km 큰 경우
		// 전회 종료거리와 운행거리를 1km 늘려 연속성을 맞춘다.
		for pos := 1; pos < len(idxs); pos++ {
			prev := &corrected[idxs[pos-1]]
			cur := corrected[idxs[pos]]
			if prev.End == nil || cur.Start == nil || *cur.Start-*prev.End != 1 {
				continue
			}
			oldEnd := *prev.End
			newEnd := oldEnd + 1
			prev.End = intPtr(newEnd)
			if prev.Start != nil {
				prev.Distance = intPtr(newEnd - *prev.Start)
			}
			prev.Correction = appendCorrection(prev.Correction, fmt.Sprintf("연속성 보정: 종료거리 %dkm → %dkm(+1km)", oldEnd, newEnd))
		}
	}
	return corrected
}

func intPtr(v int) *int { return &v }

func judgeRecordsFiltered(records []Record, filter MonthFilter, settings Settings) []Judged {
	records = applyContinuityCorrections(records)
	var out []Judged
	groups := map[string][]int{}
	for i, r := range records {
		groups[r.Vehicle] = append(groups[r.Vehicle], i)
	}
	for _, idxs := range groups {
		sort.SliceStable(idxs, func(a, b int) bool {
			ra, rb := records[idxs[a]], records[idxs[b]]
			if ra.Date != rb.Date {
				return ra.Date < rb.Date
			}
			return val(ra.Start, 1<<30) < val(rb.Start, 1<<30)
		})
		prev := -1
		normalHistory := []float64{}
		for _, idx := range idxs {
			r := records[idx]
			judge := "정상"
			var prevRange, delta, decreasePtr *int
			var suspicionPtr, stablePtr, basePtr, rawCorrPtr, dynOffsetPtr, allowedPtr *float64
			vehicleCorrection := 1.0

			// 차량보정치는 현재 운행을 포함하지 않고, 같은 차량의 이전 정상 기록만 사용한다.
			// 장기 기준은 이전 정상 전체 중앙값, 최근 기준은 이전 정상 N건 중앙값이다.
			if len(normalHistory) >= settings.MinBaseRecords && len(normalHistory) >= settings.RecentBaseRecords {
				stable := medianFloat(normalHistory)
				recentVals := recentFloatValues(normalHistory, settings.RecentBaseRecords)
				recent := medianFloat(recentVals)
				if stable > 0 && recent > 0 {
					stablePtr = &stable
					basePtr = &recent
					raw, corr := calcVehicleCorrection(stable, recent, settings)
					rc := raw
					rawCorrPtr = &rc
					vehicleCorrection = corr
				}
			}

			if r.Date == "" || r.Driver == "" || r.Driver == "(미기재)" || r.Vehicle == "" || r.Start == nil || r.End == nil || r.Distance == nil || r.Range == nil {
				judge = "데이터 누락"
			} else if *r.End < *r.Start {
				judge = "종료거리가 시작거리보다 작음"
			} else if prev >= 0 && records[prev].End != nil && r.Start != nil && *r.Start > *records[prev].End {
				judge = "연속성 오류(보고 누락 된 운행)"
			} else if prev >= 0 && records[prev].End != nil && r.Start != nil && *r.Start < *records[prev].End {
				judge = "연속성 오류(중복)"
			} else if r.Distance != nil && *r.Distance == 0 {
				judge = "운행거리 오기재"
			} else if r.Distance != nil && *r.Distance >= settings.OverdriveKm {
				judge = "과다운전"
			} else {
				if prev >= 0 && records[prev].Range != nil && r.Range != nil && r.Distance != nil && *r.Distance > 0 {
					pr := *records[prev].Range
					prevRange = &pr
					de := pr - *r.Range
					delta = &de
					decrease := de
					if decrease < 0 {
						decrease = 0
					}
					decreasePtr = &decrease
					suspicion := float64(decrease) / float64(*r.Distance)
					suspicionPtr = &suspicion
					dyn := dynamicOffset(*r.Distance, settings)
					dynOffsetPtr = &dyn
					allowed := float64(*r.Distance)*settings.Multiplier/vehicleCorrection + dyn
					allowedPtr = &allowed
					judge = decreaseJudgeByDistance(*r.Distance, decrease, allowed)
				}
			}
			if r.Correction != "" && judge == "정상" {
				judge = "연속성 보정"
			}

			j := Judged{Record: r, PrevRange: prevRange, RangeDelta: delta, RangeDecrease: decreasePtr, SuspicionIndex: suspicionPtr, VehicleStableIndex: stablePtr, VehicleBaseIndex: basePtr, RawCorrection: rawCorrPtr, VehicleCorrection: vehicleCorrection, DynamicOffset: dynOffsetPtr, AllowedDecrease: allowedPtr, Judge: judge}
			if inFilter(r, filter) {
				out = append(out, j)
			}
			if shouldUseForCorrection(j) {
				normalHistory = append(normalHistory, *j.SuspicionIndex)
			}
			prev = idx
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		if out[i].Vehicle != out[j].Vehicle {
			return out[i].Vehicle < out[j].Vehicle
		}
		return val(out[i].Start, 1<<30) < val(out[j].Start, 1<<30)
	})
	return out
}

// XLSX builder
type Cell struct {
	V     interface{}
	Style int
}
type Sheet struct {
	Name      string
	Rows      [][]Cell
	Merges    []string
	Widths    map[int]float64
	Heights   map[int]float64
	RowBreaks []int
	PrintArea string
	Landscape bool
	FitHeight int
	Footer    string
}

func c(v interface{}, style int) Cell { return Cell{v, style} }

func createXLSX(path string, judged []Judged, filter MonthFilter, settings Settings, logInfo VehicleLogInfo) error {
	sheets := buildSheets(judged, filter, settings, logInfo)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	add := func(name, data string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(data))
		return err
	}
	if err := add("[Content_Types].xml", contentTypes(len(sheets))); err != nil {
		return err
	}
	add("_rels/.rels", relsRoot())
	add("xl/workbook.xml", workbookXML(sheets))
	add("xl/_rels/workbook.xml.rels", workbookRels(len(sheets)))
	add("xl/styles.xml", stylesXML())
	for i, s := range sheets {
		add(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), sheetXML(s))
	}
	return nil
}
func buildSheets(judged []Judged, filter MonthFilter, settings Settings, logInfo VehicleLogInfo) []Sheet {
	raw := Sheet{Name: "Raw Data", Widths: map[int]float64{1: 13, 2: 11, 3: 12, 4: 12, 5: 18, 6: 18, 7: 12, 8: 16, 9: 28, 10: 42}}
	raw.Rows = append(raw.Rows, cells([]interface{}{"일자", "게시시간", "운전자", "운전차량", "운행시작누적거리_km", "운행종료누적거리_km", "운행거리_km", "주행가능거리_km", "원문비고", "자동보정내역"}, 2))
	for _, r := range judged {
		raw.Rows = append(raw.Rows, cells([]interface{}{r.Date, r.PostTime, r.Driver, r.Vehicle, ptr(r.Start), ptr(r.End), ptr(r.Distance), ptr(r.Range), r.Note, r.Correction}, 3))
	}
	log := Sheet{Name: "추출 로그", Widths: map[int]float64{1: 22, 2: 40}}
	log.Rows = append(log.Rows, cells([]interface{}{"항목", "값"}, 2), cells([]interface{}{"생성일시", time.Now().Format("2006-01-02 15:04:05")}, 3), cells([]interface{}{"프로그램 버전", "v3.93"}, 3),
		cells([]interface{}{"최종 운행기록", len(judged)}, 3), cells([]interface{}{"과다 감소 기준", fmt.Sprintf("감소량 > 운행거리 × %.2f ÷ 차량보정치 + 동적오프셋", settings.Multiplier)}, 3), cells([]interface{}{"과다운전 기준", fmt.Sprintf("%dkm 이상", settings.OverdriveKm)}, 3))
	sheets := []Sheet{raw, log}
	for _, v := range []string{"아반떼", "모닝"} {
		sheets = append(sheets, vehicleSheet(v, judged))
	}
	sheets = append(sheets, reportSheet(judged, filter, settings), reasonSheet(judged), settingSheet(filter, settings))
	for _, v := range []string{"아반떼", "모닝"} {
		sheets = append(sheets, officialVehicleSheet(v, judged, filter, logInfo))
	}
	return sheets
}
func ptr(p *int) interface{} {
	if p == nil {
		return ""
	}
	return *p
}
func ptrFloat(p *float64) interface{} {
	if p == nil {
		return ""
	}
	return math.Round(*p*100) / 100
}
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
func cells(vals []interface{}, style int) []Cell {
	row := make([]Cell, len(vals))
	for i, v := range vals {
		row[i] = c(v, style)
	}
	return row
}
func vehicleSheet(vehicle string, judged []Judged) Sheet {
	s := Sheet{Name: vehicle + " 운행일지", Widths: map[int]float64{1: 6, 2: 13, 3: 11, 4: 10, 5: 10, 6: 10, 7: 10, 8: 10, 9: 12, 10: 14, 11: 14, 12: 14, 13: 10, 14: 14, 15: 14, 16: 12, 17: 12, 18: 14, 19: 34, 20: 28}}
	s.Rows = append(s.Rows, cells([]interface{}{"번호", "일자", "게시시간", "운전자", "운전차량", "시작거리", "종료거리", "운행거리", "주행가능거리", "전회 가능거리", "가능거리 변화량", "판정 감소량", "의심지수", "장기기준지수", "최근10기준지수", "원보정치", "차량보정치", "허용감소량", "판정", "비고"}, 2))
	n := 1
	for _, j := range judged {
		if j.Vehicle == vehicle {
			st := 3
			if !isNormalEquivalent(j.Judge) {
				st = 6
			}
			displayNote := mergeNotes(j.Note, j.Correction)
			s.Rows = append(s.Rows, []Cell{c(n, 3), c(j.Date, 3), c(j.PostTime, 3), c(j.Driver, 3), c(j.Vehicle, 3), c(ptr(j.Start), 3), c(ptr(j.End), 3), c(ptr(j.Distance), 3), c(ptr(j.Range), 3), c(ptr(j.PrevRange), 3), c(ptr(j.RangeDelta), 3), c(ptr(j.RangeDecrease), 3), c(ptrFloat(j.SuspicionIndex), 3), c(ptrFloat(j.VehicleStableIndex), 3), c(ptrFloat(j.VehicleBaseIndex), 3), c(ptrFloat(j.RawCorrection), 3), c(round2(j.VehicleCorrection), 3), c(ptrFloat(j.AllowedDecrease), 3), c(j.Judge, st), c(displayNote, 3)})
			n++
		}
	}
	return s
}

func fullDriverName(name string) string {
	for _, p := range people {
		if lastName(p) == name {
			return p
		}
	}
	return name
}

func fuelAndPurpose(note string) (string, interface{}, string) {
	note = strings.TrimSpace(note)
	re := regexp.MustCompile(`(?i)(?:주유|휘발유)\s*[:：]?\s*(\d+(?:\.\d+)?)\s*(?:l|ℓ|리터)`)
	matches := re.FindAllStringSubmatch(note, -1)
	var amount interface{} = ""
	if len(matches) > 0 {
		if v, err := strconv.ParseFloat(matches[len(matches)-1][1], 64); err == nil {
			if math.Mod(v, 1) == 0 {
				amount = int(v)
			} else {
				amount = v
			}
		}
	}
	purpose := re.ReplaceAllString(note, "")
	purpose = strings.Trim(purpose, " /,;·|-\t")
	purpose = strings.Join(strings.Fields(purpose), " ")
	if purpose == "" {
		purpose = "공사인솔"
	}
	if len(matches) > 0 {
		return "휘발유", amount, purpose
	}
	return "", "", purpose
}

func logDate(date string) string {
	parts := strings.Split(date, "-")
	if len(parts) != 3 {
		return date
	}
	mo := strings.TrimLeft(parts[1], "0")
	d := strings.TrimLeft(parts[2], "0")
	if mo == "" {
		mo = "0"
	}
	if d == "" {
		d = "0"
	}
	return fmt.Sprintf("%s. %s.", mo, d)
}

func appendVehicleHeader(s *Sheet, firstPage bool, vehicle string, vehicleNo string, unit string, sequence string, month int) {
	if firstPage {
		// 원본 서식처럼 제목 위쪽 여백을 넉넉하게 둔다.
		for i := 0; i < 4; i++ {
			r := len(s.Rows) + 1
			row := cells([]interface{}{"", "", "", "", "", "", "", "", "", "", ""}, 0)
			if i == 0 {
				row[0] = c("[별지 제1호 서식]", 30)
			}
			s.Rows = append(s.Rows, row)
			s.Heights[r] = 18
		}

		r1 := len(s.Rows) + 1
		s.Rows = append(s.Rows, []Cell{c(fmt.Sprintf("( %d )월 차량 운행 일지", month), 9), c("", 9), c("", 9), c("", 9), c("", 9), c("", 9), c("", 9), c("", 9), c("", 9), c("", 9), c("", 9)})
		s.Merges = append(s.Merges, fmt.Sprintf("A%d:K%d", r1, r1))
		s.Heights[r1] = 36

		// 제목과 상단 정보 사이 간격.
		rGap := len(s.Rows) + 1
		s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", "", "", "", "", "", ""}, 0))
		s.Heights[rGap] = 14

		// 상단 정보는 본문과 분리된 별도 표로 구성한다.
		// 1행: 부대명/부서명 | 값 | 일지순번+(연-월-장) | 값
		r2 := len(s.Rows) + 1
		s.Rows = append(s.Rows, []Cell{c("부대명 / 부서명", 14), c("", 17), c(unit, 15), c("", 15), c("", 15), c("일지순번\n(연 - 월 - 장)", 14), c("", 17), c(sequence, 15), c("", 15), c("", 15), c("", 15)})
		s.Merges = append(s.Merges,
			fmt.Sprintf("A%d:B%d", r2, r2),
			fmt.Sprintf("C%d:E%d", r2, r2),
			fmt.Sprintf("F%d:G%d", r2, r2),
			fmt.Sprintf("H%d:K%d", r2, r2))
		s.Heights[r2] = 42

		// 2행: 장비명 | 값 | 차량번호 | 값
		r3 := len(s.Rows) + 1
		s.Rows = append(s.Rows, []Cell{c("장비명", 14), c("", 17), c(vehicle, 15), c("", 15), c("", 15), c("차량번호", 14), c("", 17), c(vehicleNo, 15), c("", 15), c("", 15), c("", 15)})
		s.Merges = append(s.Merges,
			fmt.Sprintf("A%d:B%d", r3, r3),
			fmt.Sprintf("C%d:E%d", r3, r3),
			fmt.Sprintf("F%d:G%d", r3, r3),
			fmt.Sprintf("H%d:K%d", r3, r3))
		s.Heights[r3] = 30

		// 상단 정보표와 본문 표 사이에 빈 줄을 둔다.
		rInfoGap := len(s.Rows) + 1
		s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", "", "", "", "", "", ""}, 0))
		s.Heights[rInfoGap] = 14
	}

	rh1 := len(s.Rows) + 1
	s.Rows = append(s.Rows, []Cell{c("일자", 17), c("출발지", 17), c("", 17), c("도착지", 17), c("", 17), c("주행거리\n(Km)", 17), c("운행목적", 17), c("총 주행거리\n(Km)", 17), c("유류사용", 17), c("", 17), c("운전자\n(계급, 성명)", 17)})
	rh2 := len(s.Rows) + 1
	s.Rows = append(s.Rows, []Cell{c("", 17), c("장소", 17), c("시간", 17), c("장소", 17), c("시간", 17), c("", 17), c("", 17), c("", 17), c("사용유류", 17), c("보충량(ℓ)", 17), c("", 17)})
	s.Merges = append(s.Merges,
		fmt.Sprintf("A%d:A%d", rh1, rh2), fmt.Sprintf("B%d:C%d", rh1, rh1), fmt.Sprintf("D%d:E%d", rh1, rh1),
		fmt.Sprintf("F%d:F%d", rh1, rh2), fmt.Sprintf("G%d:G%d", rh1, rh2), fmt.Sprintf("H%d:H%d", rh1, rh2),
		fmt.Sprintf("I%d:J%d", rh1, rh1), fmt.Sprintf("K%d:K%d", rh1, rh2))
	s.Heights[rh1] = 28
	s.Heights[rh2] = 24
}

func officialBorderStyle(top, bottom, left, right, leftAligned bool) int {
	// 공식 보고 표 전용 테두리 스타일.
	// 외곽 또는 머리글 하단은 medium, 내부선은 thin으로 유지한다.
	if leftAligned {
		switch {
		case top && left:
			return 23
		case top && right:
			return 24
		case bottom && left:
			return 25
		case bottom && right:
			return 26
		case top:
			return 27
		case bottom:
			return 28
		case left:
			return 29 // 내부 얇은선 + 좌측 정렬(운행목적)
		case right:
			return 29
		default:
			return 18
		}
	}
	switch {
	case top && left:
		return 23
	case top && right:
		return 24
	case bottom && left:
		return 25
	case bottom && right:
		return 26
	case top:
		return 19
	case bottom:
		return 20
	case left:
		return 21
	case right:
		return 22
	default:
		return 17
	}
}

func applyOfficialBorderRange(s *Sheet, startRow int, endRow int, headerBottomRow int) {
	for r := startRow; r <= endRow; r++ {
		for col := 1; col <= 11; col++ {
			if r-1 < 0 || r-1 >= len(s.Rows) || col-1 >= len(s.Rows[r-1]) {
				continue
			}
			top := r == startRow
			bottom := r == endRow || (headerBottomRow > 0 && r == headerBottomRow)
			left := col == 1
			right := col == 11
			leftAligned := col == 7 && r > headerBottomRow
			s.Rows[r-1][col-1].Style = officialBorderStyle(top, bottom, left, right, leftAligned)
		}
	}
}

func officialVehicleSheet(vehicle string, judged []Judged, filter MonthFilter, logInfo VehicleLogInfo) Sheet {
	s := Sheet{
		Name: vehicle + " 공식 보고",
		// 운행목적(G)과 총 주행거리(H)를 넓히고, 장소·시간·유류 열은 소폭 줄였다.
		Widths:  map[int]float64{1: 8.5, 2: 11.5, 3: 7, 4: 11.5, 5: 7, 6: 9.5, 7: 31, 8: 14.5, 9: 9.5, 10: 9.5, 11: 16.5},
		Heights: map[int]float64{}, Landscape: true, FitHeight: 0,
		Footer: "&P / &N",
	}
	vehicleNo := logInfo.MorningNumber
	if vehicle == "아반떼" {
		vehicleNo = logInfo.AvanteNumber
	}
	year, month := reportYearMonth(judged, filter)

	var records []Judged
	for _, j := range judged {
		if j.Vehicle == vehicle {
			records = append(records, j)
		}
	}

	const firstPageRows = 7
	const continuationRows = 14
	totalPages := 1
	if len(records) > firstPageRows {
		remaining := len(records) - firstPageRows
		totalPages += (remaining + continuationRows - 1) / continuationRows
	}
	sequence := fmt.Sprintf("%04d - %02d - %02d", year, month, totalPages)

	index := 0
	page := 0
	for index < len(records) || page == 0 {
		first := page == 0
		appendVehicleHeader(&s, first, vehicle, vehicleNo, logInfo.UnitDepartment, sequence, month)
		// appendVehicleHeader 직후 마지막 두 행이 본문 머리글이다.
		tableStartRow := len(s.Rows) - 1
		headerBottomRow := len(s.Rows)
		if first {
			// 첫 페이지 상단 정보표(A7:K8)는 외곽만 굵고 내부는 얇게 한다.
			applyOfficialBorderRange(&s, 7, 8, 0)
		}
		capacity := continuationRows
		if first {
			capacity = firstPageRows
		}
		for slot := 0; slot < capacity; slot++ {
			row := []Cell{c("", 17), c("", 17), c("", 17), c("", 17), c("", 17), c("", 17), c("", 18), c("", 17), c("", 17), c("", 17), c("", 17)}
			if index < len(records) {
				r := records[index]
				location := "기지 내"
				if r.Distance != nil && *r.Distance >= 100 {
					location = ""
				}
				fuel, amount, purpose := fuelAndPurpose(r.Note)
				row = []Cell{c(logDate(r.Date), 17), c(location, 17), c("", 17), c(location, 17), c("", 17), c(ptr(r.Distance), 17), c(purpose, 18), c(ptr(r.End), 17), c(fuel, 17), c(amount, 17), c(fullDriverName(r.Driver), 17)}
				index++
			}
			s.Rows = append(s.Rows, row)
			s.Heights[len(s.Rows)] = 30
		}
		applyOfficialBorderRange(&s, tableStartRow, len(s.Rows), headerBottomRow)
		page++
		if index < len(records) {
			// 현재 페이지 마지막 행 뒤에서 정확히 나눈다.
			s.RowBreaks = append(s.RowBreaks, len(s.Rows))
		}
	}
	s.PrintArea = fmt.Sprintf("$A$1:$K$%d", len(s.Rows))
	return s
}

func reportSheet(judged []Judged, filter MonthFilter, settings Settings) Sheet {
	s := Sheet{Name: "월간 보고서", Widths: map[int]float64{1: 12, 2: 18, 3: 12, 4: 14, 5: 12, 6: 12}}
	s.Merges = []string{"A1:F1", "A6:F6", "A13:F13", "A24:F24", "A38:F38", "A26:E26", "A27:E27", "A28:E28", "A29:E29", "A30:E30", "A31:E31", "A32:E32", "A33:E33", "A34:E34", "A35:E35", "A36:E36"}
	year, month := reportYearMonth(judged, filter)
	s.Rows = append(s.Rows, []Cell{c(fmt.Sprintf("%d년 %d월 차량 운행 월간 보고서", year, month), 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1)})
	s.Rows = append(s.Rows, cells([]interface{}{"작성연도", fmt.Sprintf("%d년", year), "작성일자", fmt.Sprintf("%d월", month), "작성자", "6급 장현익"}, 3))
	s.Rows = append(s.Rows, cells([]interface{}{"부서", "기술관리실", "기간", period(judged), "", ""}, 3))
	s.Rows = append(s.Rows, cells([]interface{}{"비고", "", "", "", "", ""}, 3))
	s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", ""}, 0))
	s.Rows = append(s.Rows, []Cell{c("1. 월간 및 차량별 운행실적", 8), c("", 8), c("", 8), c("", 8), c("", 8), c("", 8)})
	s.Rows = append(s.Rows, cells([]interface{}{"월간 요약", "", "차량별 운행실적", "", "", "비고"}, 2))
	total := len(judged)
	totalKm := sumKm(judged, "")
	normal := countNormalEquivalent(judged)
	abnormal := total - normal
	vehicleRows := [][]interface{}{{"총 운행횟수", total, "차량", "운행횟수", "총 운행거리(km)", ""}, {"총 운행거리(km)", totalKm, "아반떼", countVehicle(judged, "아반떼"), sumKm(judged, "아반떼"), ""}, {"정상 판정", normal, "모닝", countVehicle(judged, "모닝"), sumKm(judged, "모닝"), ""}, {"정상 외 판정", abnormal, "기타", countOtherVehicle(judged), sumOtherKm(judged), ""}}
	for _, r := range vehicleRows {
		row := cells(r, 3)
		if r[0] == "정상 외 판정" {
			row[1].Style = 6
		}
		s.Rows = append(s.Rows, row)
	}
	s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", ""}, 0))
	s.Rows = append(s.Rows, []Cell{c("2. 개인별 운행실적", 8), c("", 8), c("", 8), c("", 8), c("", 8), c("", 8)})
	s.Rows = append(s.Rows, cells([]interface{}{"순번", "성명", "운행횟수", "총 운행거리(km)", "정상 외 건수", "비고"}, 2))
	for i, p := range people {
		name := lastName(p)
		cnt := countDriver(judged, name)
		km := sumDriverKm(judged, name)
		ab := countDriverAbnormal(judged, name)
		row := cells([]interface{}{i + 1, p, cnt, km, ab, ""}, 3)
		if ab > 0 {
			row[4].Style = 6
		}
		s.Rows = append(s.Rows, row)
	}
	s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", ""}, 0))
	s.Rows = append(s.Rows, []Cell{c("3. 판정 기준", 8), c("", 8), c("", 8), c("", 8), c("", 8), c("", 8)})
	s.Rows = append(s.Rows, cells([]interface{}{"판정", "", "", "", "", "건수"}, 2))
	cats := []string{"정상", "연속성 보정", "데이터 누락", "종료거리가 시작거리보다 작음", "연속성 오류(보고 누락 된 운행)", "연속성 오류(중복)", "운행거리 오기재", "과다운전", "단거리 주행가능거리 과다 감소", "운행거리 대비 주행가능거리 감소 주의", "운행거리 대비 주행가능거리 과다 감소"}
	for _, cat := range cats {
		catCount := countJudge(judged, cat)
		if cat == "정상" {
			catCount += countJudge(judged, "단거리 과다감소 주의")
		}
		row := cells([]interface{}{cat, "", "", "", "", catCount}, 3)
		if cat != "정상" && catCount > 0 {
			row[5].Style = 6
		}
		s.Rows = append(s.Rows, row)
	}
	s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", ""}, 0))
	s.Rows = append(s.Rows, []Cell{c("4. 기타 사항", 8), c("", 8), c("", 8), c("", 8), c("", 8), c("", 8)})
	for i := 0; i < 8; i++ {
		s.Rows = append(s.Rows, cells([]interface{}{"", "", "", "", "", ""}, 3))
	}
	return s
}
func reportYearMonth(j []Judged, filter MonthFilter) (int, int) {
	if filter.Enabled {
		return filter.Year, filter.Month
	}
	for _, r := range j {
		y, m, ok := yearMonth(r.Date)
		if ok {
			return y, m
		}
	}
	now := time.Now()
	return now.Year(), int(now.Month())
}
func period(j []Judged) string {
	min, max := "", ""
	for _, r := range j {
		if r.Date == "" {
			continue
		}
		if min == "" || r.Date < min {
			min = r.Date
		}
		if max == "" || r.Date > max {
			max = r.Date
		}
	}
	if min == "" {
		return ""
	}
	return fmt.Sprintf("'%s ~ '%s", shortDate(min), shortDate(max))
}
func shortDate(d string) string {
	parts := strings.Split(d, "-")
	if len(parts) != 3 {
		return d
	}
	return fmt.Sprintf("%s. %s. %s", parts[0][2:], strings.TrimLeft(parts[1], "0"), strings.TrimLeft(parts[2], "0"))
}
func lastName(p string) string {
	a := strings.Fields(p)
	if len(a) == 0 {
		return p
	}
	return a[len(a)-1]
}
func sumKm(j []Judged, v string) int {
	s := 0
	for _, r := range j {
		if v == "" || r.Vehicle == v {
			if r.Distance != nil {
				s += *r.Distance
			}
		}
	}
	return s
}
func countVehicle(j []Judged, v string) int {
	n := 0
	for _, r := range j {
		if r.Vehicle == v {
			n++
		}
	}
	return n
}
func countOtherVehicle(j []Judged) int {
	n := 0
	for _, r := range j {
		if r.Vehicle != "아반떼" && r.Vehicle != "모닝" {
			n++
		}
	}
	return n
}
func sumOtherKm(j []Judged) int {
	s := 0
	for _, r := range j {
		if r.Vehicle != "아반떼" && r.Vehicle != "모닝" && r.Distance != nil {
			s += *r.Distance
		}
	}
	return s
}
func countNormalEquivalent(j []Judged) int {
	n := 0
	for _, r := range j {
		if isNormalEquivalent(r.Judge) {
			n++
		}
	}
	return n
}

func countJudge(j []Judged, cat string) int {
	n := 0
	for _, r := range j {
		if r.Judge == cat {
			n++
		}
	}
	return n
}
func countDriver(j []Judged, name string) int {
	n := 0
	for _, r := range j {
		if r.Driver == name {
			n++
		}
	}
	return n
}
func sumDriverKm(j []Judged, name string) int {
	s := 0
	for _, r := range j {
		if r.Driver == name && r.Distance != nil {
			s += *r.Distance
		}
	}
	return s
}
func countDriverAbnormal(j []Judged, name string) int {
	n := 0
	for _, r := range j {
		if r.Driver == name && !isNormalEquivalent(r.Judge) {
			n++
		}
	}
	return n
}
func reasonSheet(judged []Judged) Sheet {
	s := Sheet{Name: "이상 사유", Widths: map[int]float64{1: 12, 2: 8, 3: 10, 4: 9, 5: 9, 6: 10, 7: 14, 8: 10, 9: 10, 10: 12, 11: 14, 12: 38}}
	s.Merges = []string{"A1:L1"}
	s.Rows = append(s.Rows, []Cell{c("이상 운행 세부내역 및 사유 작성", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1), c("", 1)})
	s.Rows = append(s.Rows, cells([]interface{}{"일자", "운전자", "운전차량", "시작거리", "종료거리", "운행거리", "주행가능거리", "판정감소", "의심지수", "차량보정", "허용감소", "판정 및 사유"}, 2))
	for _, r := range judged {
		if !isNormalEquivalent(r.Judge) {
			judgeText := r.Judge
			if strings.Contains(r.Judge, "감소") {
				dist := ""
				decrease := ""
				allowed := ""
				if r.Distance != nil {
					dist = fmt.Sprintf("%dkm", *r.Distance)
				}
				if r.RangeDecrease != nil {
					decrease = fmt.Sprintf("%dkm", *r.RangeDecrease)
				}
				if r.AllowedDecrease != nil {
					allowed = fmt.Sprintf("%.1fkm", *r.AllowedDecrease)
				}
				judgeText = fmt.Sprintf("%s\n운행거리: %s / 판정감소: %s / 허용감소: %s", r.Judge, dist, decrease, allowed)
			}
			s.Rows = append(s.Rows, []Cell{c(r.Date, 3), c(r.Driver, 3), c(r.Vehicle, 3), c(ptr(r.Start), 3), c(ptr(r.End), 3), c(ptr(r.Distance), 3), c(ptr(r.Range), 3), c(ptr(r.RangeDecrease), 3), c(ptrFloat(r.SuspicionIndex), 3), c(round2(r.VehicleCorrection), 3), c(ptrFloat(r.AllowedDecrease), 3), c(judgeText+"\n사유: "+strings.TrimSpace(r.Note), 7)})
		}
	}
	for i := 0; i < 12; i++ {
		s.Rows = append(s.Rows, []Cell{c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 3), c("", 7)})
	}
	return s
}
func settingSheet(filter MonthFilter, settings Settings) Sheet {
	s := Sheet{Name: "설정", Widths: map[int]float64{1: 24, 2: 65}}
	target := "전체"
	if filter.Enabled {
		target = fmt.Sprintf("%04d년 %d월", filter.Year, filter.Month)
	}
	s.Rows = append(s.Rows, cells([]interface{}{"항목", "값"}, 2), cells([]interface{}{"프로그램 버전", "v3.93"}, 3), cells([]interface{}{"대상월", target}, 3), cells([]interface{}{"배율 B", settings.Multiplier}, 3), cells([]interface{}{"기본여유 O(km)", settings.MarginKm}, 3), cells([]interface{}{"과다운전 기준(km)", settings.OverdriveKm}, 3), cells([]interface{}{"기본 의심지수 S0", settings.BaseSuspicionIndex}, 3), cells([]interface{}{"보정 최대폭 A", settings.CorrectionWidth}, 3), cells([]interface{}{"보정 민감도 K", settings.CorrectionSensitivity}, 3), cells([]interface{}{"오프셋 기준거리 D0(km)", settings.OffsetBaseDistance}, 3), cells([]interface{}{"보정치 산정 최소 정상 기록", settings.MinBaseRecords}, 3), cells([]interface{}{"차량보정치 최근 정상 기록 수", settings.RecentBaseRecords}, 3), cells([]interface{}{"과다 감소 기준", "판정감소량 > 운행거리 × B ÷ 차량보정치 + 동적오프셋"}, 3), cells([]interface{}{"판정감소량", "MAX(0, 전회 주행가능거리 - 현재 주행가능거리)"}, 3), cells([]interface{}{"차량보정치", "현재 운행 이전 정상 기록만 사용. 장기 전체 중앙값 ÷ 최근 정상 N건 중앙값"}, 3), cells([]interface{}{"동적오프셋", "기본여유 × MIN(1, 운행거리 ÷ 오프셋 기준거리)"}, 3), cells([]interface{}{"단거리 판정", "1~5km: 감소량 12km 이하 정상, 25km 이하 주의, 초과 과다 감소"}, 3), cells([]interface{}{"보정치 학습 기준", "정상 판정 중 운행거리 4km 이상만 최근 정상 10건 학습에 반영"}, 3), cells([]interface{}{"병합 기준", "차량·시작거리·종료거리·주행가능거리가 같으면 날짜가 달라도 1건으로 병합하고, 이른 날짜와 비고를 보존"}, 3), cells([]interface{}{"연속성 자동보정", "현재 시작거리 - 전회 종료거리 = 1km이면 전회 종료거리와 운행거리를 +1km 보정"}, 3), cells([]interface{}{"시작거리 자동보정", "현재 시작거리가 전회 시작거리와 같고 현재 종료거리=다음 시작거리이며 주행가능거리가 변한 경우, 현재 시작거리를 전회 종료거리로 보정"}, 3), cells([]interface{}{"단거리 주의 보고처리", "단거리 과다감소 주의는 운행일지 판정은 유지하되 월간 집계와 이상 사유에서는 정상으로 처리"}, 3), cells([]interface{}{"참고 데이터", "대상월 이전 기록도 차량보정치 학습에는 사용하되, 대상월 필터가 있으면 엑셀 출력은 대상월만 표시"}, 3))
	return s
}

func contentTypes(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= n; i++ {
		b.WriteString(fmt.Sprintf(`<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i))
	}
	b.WriteString(`</Types>`)
	return b.String()
}
func relsRoot() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`
}
func workbookXML(sheets []Sheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, s := range sheets {
		b.WriteString(fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, escAttr(s.Name), i+1, i+1))
	}
	b.WriteString(`</sheets>`)
	hasPrint := false
	for _, s := range sheets {
		if s.PrintArea != "" {
			hasPrint = true
			break
		}
	}
	if hasPrint {
		b.WriteString(`<definedNames>`)
		for i, s := range sheets {
			if s.PrintArea == "" {
				continue
			}
			name := strings.ReplaceAll(s.Name, "'", "''")
			b.WriteString(fmt.Sprintf(`<definedName name="_xlnm.Print_Area" localSheetId="%d">'%s'!%s</definedName>`, i, esc(name), s.PrintArea))
		}
		b.WriteString(`</definedNames>`)
	}
	b.WriteString(`</workbook>`)
	return b.String()
}

func workbookRels(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= n; i++ {
		b.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i))
	}
	b.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, n+1))
	b.WriteString(`</Relationships>`)
	return b.String()
}
func stylesXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="7"><font><sz val="10"/><name val="맑은 고딕"/></font><font><b/><sz val="16"/><color rgb="FFFFFFFF"/><name val="맑은 고딕"/></font><font><b/><sz val="10"/><name val="맑은 고딕"/></font><font><sz val="10"/><name val="맑은 고딕"/></font><font><b/><sz val="18"/><name val="맑은 고딕"/></font><font><b/><sz val="11"/><name val="맑은 고딕"/></font><font><sz val="10"/><name val="맑은 고딕"/></font></fonts><fills count="8"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FF1F4E79"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFD9EAF7"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFFFE4D6"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFFFF2CC"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFEAF3F8"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFF2F2F2"/></patternFill></fill></fills><borders count="12"><border><left/><right/><top/><bottom/><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="medium"><color rgb="FF000000"/></left><right style="medium"><color rgb="FF000000"/></right><top style="medium"><color rgb="FF000000"/></top><bottom style="medium"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="medium"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="medium"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="medium"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="medium"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="medium"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="medium"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="medium"><color rgb="FF000000"/></right><top style="medium"><color rgb="FF000000"/></top><bottom style="thin"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="medium"><color rgb="FF000000"/></left><right style="thin"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="medium"><color rgb="FF000000"/></bottom><diagonal/></border><border><left style="thin"><color rgb="FF000000"/></left><right style="medium"><color rgb="FF000000"/></right><top style="thin"><color rgb="FF000000"/></top><bottom style="medium"><color rgb="FF000000"/></bottom><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="31"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center"/></xf><xf numFmtId="0" fontId="2" fillId="3" borderId="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="0" fillId="0" borderId="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="0" fillId="0" borderId="1" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="0" fillId="0" borderId="1" applyBorder="1" applyAlignment="1"><alignment horizontal="right" vertical="center"/></xf><xf numFmtId="0" fontId="0" fillId="4" borderId="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="0" fillId="5" borderId="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="top" wrapText="1"/></xf><xf numFmtId="0" fontId="2" fillId="6" borderId="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center"/></xf><xf numFmtId="0" fontId="4" fillId="0" borderId="0" applyAlignment="1"><alignment horizontal="center" vertical="center"/></xf><xf numFmtId="0" fontId="5" fillId="0" borderId="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="5" fillId="7" borderId="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="1" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="5" fillId="0" borderId="2" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="2" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="2" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="3" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="3" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="4" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="5" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="6" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="7" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="8" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="9" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="10" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="11" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="4" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="5" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="3" applyBorder="1" applyAlignment="1"><alignment horizontal="left" vertical="center" wrapText="1"/></xf><xf numFmtId="0" fontId="6" fillId="0" borderId="0" applyAlignment="1"><alignment horizontal="left" vertical="center"/></xf></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`
}

func sheetXML(s Sheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	if s.Landscape {
		b.WriteString(`<sheetPr><pageSetUpPr fitToPage="1"/></sheetPr>`)
	}
	if len(s.Widths) > 0 {
		b.WriteString(`<cols>`)
		cols := make([]int, 0, len(s.Widths))
		for k := range s.Widths {
			cols = append(cols, k)
		}
		sort.Ints(cols)
		for _, i := range cols {
			b.WriteString(fmt.Sprintf(`<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, i, i, s.Widths[i]))
		}
		b.WriteString(`</cols>`)
	}
	b.WriteString(`<sheetData>`)
	for r, row := range s.Rows {
		h := ""
		if s.Heights != nil && s.Heights[r+1] > 0 {
			h = fmt.Sprintf(` ht="%.1f" customHeight="1"`, s.Heights[r+1])
		}
		b.WriteString(fmt.Sprintf(`<row r="%d"%s>`, r+1, h))
		for cidx, cell := range row {
			ref := fmt.Sprintf("%s%d", colName(cidx+1), r+1)
			writeCell(&b, ref, cell)
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData>`)
	if len(s.Merges) > 0 {
		b.WriteString(fmt.Sprintf(`<mergeCells count="%d">`, len(s.Merges)))
		for _, m := range s.Merges {
			b.WriteString(fmt.Sprintf(`<mergeCell ref="%s"/>`, m))
		}
		b.WriteString(`</mergeCells>`)
	}
	if s.Landscape {
		b.WriteString(`<printOptions horizontalCentered="1" verticalCentered="0"/><pageMargins left="0.25" right="0.25" top="0.35" bottom="0.35" header="0.15" footer="0.15"/>`)
		b.WriteString(fmt.Sprintf(`<pageSetup paperSize="9" orientation="landscape" fitToWidth="1" fitToHeight="%d" horizontalDpi="300" verticalDpi="300"/>`, s.FitHeight))
		if s.Footer != "" {
			b.WriteString(fmt.Sprintf(`<headerFooter><oddFooter>&amp;C%s</oddFooter></headerFooter>`, esc(s.Footer)))
		}
	} else {
		b.WriteString(`<pageMargins left="0.3" right="0.3" top="0.5" bottom="0.5" header="0.2" footer="0.2"/><pageSetup paperSize="9" orientation="landscape" fitToWidth="1" fitToHeight="1"/>`)
	}
	// SpreadsheetML 스키마상 rowBreaks는 pageSetup 뒤에 와야 한다.
	if len(s.RowBreaks) > 0 {
		b.WriteString(fmt.Sprintf(`<rowBreaks count="%d" manualBreakCount="%d">`, len(s.RowBreaks), len(s.RowBreaks)))
		for _, br := range s.RowBreaks {
			b.WriteString(fmt.Sprintf(`<brk id="%d" min="0" max="16383" man="1"/>`, br))
		}
		b.WriteString(`</rowBreaks>`)
	}
	b.WriteString(`</worksheet>`)
	return b.String()
}
func writeCell(b *strings.Builder, ref string, cell Cell) {
	style := ""
	if cell.Style > 0 {
		style = fmt.Sprintf(` s="%d"`, cell.Style)
	}
	switch v := cell.V.(type) {
	case int:
		b.WriteString(fmt.Sprintf(`<c r="%s"%s><v>%d</v></c>`, ref, style, v))
	case float64:
		b.WriteString(fmt.Sprintf(`<c r="%s"%s><v>%g</v></c>`, ref, style, v))
	case string:
		if v == "" {
			b.WriteString(fmt.Sprintf(`<c r="%s"%s/>`, ref, style))
		} else {
			b.WriteString(fmt.Sprintf(`<c r="%s" t="inlineStr"%s><is><t>%s</t></is></c>`, ref, style, esc(v)))
		}
	default:
		b.WriteString(fmt.Sprintf(`<c r="%s"%s/>`, ref, style))
	}
}
func colName(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}
func esc(s string) string     { var buf bytes.Buffer; xml.EscapeText(&buf, []byte(s)); return buf.String() }
func escAttr(s string) string { return strings.ReplaceAll(esc(s), "'", "&apos;") }

// ensure io imported used
var _ io.Reader


type WebRequest struct {
    Text string `json:"text"`
    Year int `json:"year"`
    Month int `json:"month"`
    UnitDepartment string `json:"unitDepartment"`
    AvanteNumber string `json:"avanteNumber"`
    MorningNumber string `json:"morningNumber"`
    Multiplier float64 `json:"multiplier"`
    MarginKm float64 `json:"marginKm"`
    OverdriveKm int `json:"overdriveKm"`
    BaseSuspicionIndex float64 `json:"baseSuspicionIndex"`
    CorrectionWidth float64 `json:"correctionWidth"`
    CorrectionSensitivity float64 `json:"correctionSensitivity"`
    OffsetBaseDistance float64 `json:"offsetBaseDistance"`
    MinBaseRecords int `json:"minBaseRecords"`
    RecentBaseRecords int `json:"recentBaseRecords"`
}

type WebResponse struct {
    OK bool `json:"ok"`
    Error string `json:"error,omitempty"`
    FileName string `json:"fileName,omitempty"`
    Base64 string `json:"base64,omitempty"`
    RecordCount int `json:"recordCount,omitempty"`
}

func createXLSXBytes(judged []Judged, filter MonthFilter, settings Settings, logInfo VehicleLogInfo) ([]byte, error) {
    sheets := buildSheets(judged, filter, settings, logInfo)
    var buf bytes.Buffer
    zw := zip.NewWriter(&buf)
    add := func(name, data string) error {
        w, err := zw.Create(name)
        if err != nil { return err }
        _, err = w.Write([]byte(data))
        return err
    }
    if err := add("[Content_Types].xml", contentTypes(len(sheets))); err != nil { return nil, err }
    if err := add("_rels/.rels", relsRoot()); err != nil { return nil, err }
    if err := add("xl/workbook.xml", workbookXML(sheets)); err != nil { return nil, err }
    if err := add("xl/_rels/workbook.xml.rels", workbookRels(len(sheets))); err != nil { return nil, err }
    if err := add("xl/styles.xml", stylesXML()); err != nil { return nil, err }
    for i, s := range sheets {
        if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), sheetXML(s)); err != nil { return nil, err }
    }
    if err := zw.Close(); err != nil { return nil, err }
    return buf.Bytes(), nil
}

func webGenerateExcel(this js.Value, args []js.Value) interface{} {
    resp := WebResponse{}
    if len(args) == 0 {
        resp.Error = "입력값이 없습니다."
        b, _ := json.Marshal(resp)
        return string(b)
    }
    var req WebRequest
    if err := json.Unmarshal([]byte(args[0].String()), &req); err != nil {
        resp.Error = "입력값 해석 오류: " + err.Error()
        b, _ := json.Marshal(resp)
        return string(b)
    }
    if strings.TrimSpace(req.Text) == "" {
        resp.Error = "카카오톡 TXT 파일을 선택하거나 내용을 붙여넣어 주세요."
        b, _ := json.Marshal(resp)
        return string(b)
    }
    records := parseRecords(req.Text)
    if len(records) == 0 {
        resp.Error = "차량 운행 기록을 찾지 못했습니다. 카카오톡 내보내기 형식을 확인해 주세요."
        b, _ := json.Marshal(resp)
        return string(b)
    }
    filter := MonthFilter{}
    if req.Year > 0 && req.Month >= 1 && req.Month <= 12 {
        filter = MonthFilter{Year:req.Year, Month:req.Month, Enabled:true}
    } else {
        y,m := latestYearMonth(records)
        if y > 0 && m > 0 { filter = MonthFilter{Year:y, Month:m, Enabled:true} }
    }
    settings := Settings{
        Multiplier:req.Multiplier, MarginKm:req.MarginKm, OverdriveKm:req.OverdriveKm,
        BaseSuspicionIndex:req.BaseSuspicionIndex, CorrectionWidth:req.CorrectionWidth,
        CorrectionSensitivity:req.CorrectionSensitivity, OffsetBaseDistance:req.OffsetBaseDistance,
        MinBaseRecords:req.MinBaseRecords, RecentBaseRecords:req.RecentBaseRecords,
    }
    if settings.Multiplier <= 0 { settings.Multiplier = defaultMultiplier }
    if settings.MarginKm < 0 { settings.MarginKm = defaultMarginKm }
    if settings.OverdriveKm <= 0 { settings.OverdriveKm = defaultOverdriveKm }
    if settings.BaseSuspicionIndex <= 0 { settings.BaseSuspicionIndex = defaultBaseSuspicionIndex }
    if settings.CorrectionWidth < 0 { settings.CorrectionWidth = defaultCorrectionWidth }
    if settings.CorrectionSensitivity < 0 { settings.CorrectionSensitivity = defaultCorrectionSensitivity }
    if settings.OffsetBaseDistance <= 0 { settings.OffsetBaseDistance = defaultOffsetBaseDistance }
    if settings.MinBaseRecords <= 0 { settings.MinBaseRecords = defaultMinBaseRecords }
    if settings.RecentBaseRecords <= 0 { settings.RecentBaseRecords = defaultRecentBaseRecords }
    info := VehicleLogInfo{UnitDepartment:strings.TrimSpace(req.UnitDepartment), AvanteNumber:strings.TrimSpace(req.AvanteNumber), MorningNumber:strings.TrimSpace(req.MorningNumber)}
    if info.UnitDepartment == "" { info.UnitDepartment = "기술관리실" }
    if info.AvanteNumber == "" { info.AvanteNumber = "175허5481" }
    if info.MorningNumber == "" { info.MorningNumber = "175허5506" }
    judged := judgeRecordsFiltered(records, filter, settings)
    if len(judged) == 0 {
        resp.Error = "선택한 연·월에 해당하는 기록이 없습니다."
        b, _ := json.Marshal(resp)
        return string(b)
    }
    data, err := createXLSXBytes(judged, filter, settings, info)
    if err != nil {
        resp.Error = "엑셀 생성 오류: " + err.Error()
        b, _ := json.Marshal(resp)
        return string(b)
    }
    filename := "차량운행보고서.xlsx"
    if filter.Enabled { filename = fmt.Sprintf("%04d년_%02d월_차량운행보고서.xlsx", filter.Year, filter.Month) }
    resp = WebResponse{OK:true, FileName:filename, Base64:base64.StdEncoding.EncodeToString(data), RecordCount:len(judged)}
    b, _ := json.Marshal(resp)
    return string(b)
}

func main() {
    js.Global().Set("vehicleManagerGenerateExcel", js.FuncOf(webGenerateExcel))
    select {}
}
