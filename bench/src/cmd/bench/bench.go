package main

import (
	"bench/counter"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"sort"
	"strings"
	"time"

	"bench"
)

var (
	benchDuration    time.Duration = time.Minute
	preTestOnly      bool
	noLevelup        bool
	checkFuncs       []benchFunc
	loadFuncs        []benchFunc
	loadLevelUpFuncs []benchFunc
	loadLogs         []string

	pprofPort int = 16060
)

type benchFunc struct {
	Name string
	Func func(ctx context.Context, state *bench.State) error
}

func addCheckFunc(f benchFunc) {
	checkFuncs = append(checkFuncs, f)
}

func addLoadFunc(weight int, f benchFunc) {
	for i := 0; i < weight; i++ {
		loadFuncs = append(loadFuncs, f)
	}
}

func addLoadLevelUpFunc(weight int, f benchFunc) {
	for i := 0; i < weight; i++ {
		loadLevelUpFuncs = append(loadLevelUpFuncs, f)
	}
}

func requestInitialize(targetHost string) error {
	u, _ := url.Parse("/initialize")
	u.Scheme = "http"
	u.Host = targetHost

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", bench.UserAgent)
	req.Host = bench.TorbAppHost

	client := &http.Client{
		Timeout: bench.InitializeTimeout,
	}

	res, err := client.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()
	_, err = io.Copy(ioutil.Discard, res.Body)
	if err != nil {
		return err
	}

	if !(200 <= res.StatusCode && res.StatusCode < 300) {
		return fmt.Errorf("Unexpected status code: %d", res.StatusCode)
	}

	return nil
}

// 負荷を掛ける前にアプリが最低限動作しているかをチェックする
// エラーが発生したら負荷をかけずに終了する
func preTest(ctx context.Context, state *bench.State) error {
	for _, checkFunc := range checkFuncs {
		err := checkFunc.Func(ctx, state)
		if err != nil {
			return err
		}
	}

	return nil
}

func validationMain(ctx context.Context, state *bench.State) error {
	for r := range rand.Perm(len(checkFuncs)) {
		if ctx.Err() != nil {
			return nil
		}

		t := time.Now()

		checkFunc := checkFuncs[r]
		err := checkFunc.Func(ctx, state)
		log.Println(checkFunc.Name, time.Since(t))

		isFatalError := false
		if cerr, ok := err.(*bench.CheckerError); ok {
			isFatalError = cerr.IsFatal()
		}

		// fatalError以外は見逃してあげる
		if err != nil && isFatalError {
			return err
		}

		if err != nil {
			// バリデーションシナリオを悪用してスコアブーストさせないためエラーのときは少し待つ
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil
}

func load(ctx context.Context, state *bench.State, n int) {
	for i := 0; i < n; i++ {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}

				loadFunc := loadFuncs[rand.Intn(len(loadFuncs))]
				err := loadFunc.Func(ctx, state)
				if err != nil {
					return
				}
			}
		}()
	}
}

func levelUpLoad(ctx context.Context, state *bench.State, n int) {
	for i := 0; i < n; i++ {
		for _, loadFunc := range loadLevelUpFuncs {
			go loadFunc.Func(ctx, state)
		}
	}
}

func benchmarkMain(ctx context.Context, state *bench.State) {
	load(ctx, state, 10)
	levelUpLoad(ctx, state, 1)

	beat := time.NewTicker(time.Second)
	defer beat.Stop()

	for {
		select {
		case <-beat.C:
			if noLevelup {
				continue
			}

			e, et := bench.GetLastCheckerError()
			hasRecentErr := e != nil && time.Since(et) < 5*time.Second

			path, st := bench.GetLastSlowPath()
			hasRecentSlowPath := path != "" && time.Since(st) < 5*time.Second

			now := time.Now().Format("01/02 15:04:05")

			if hasRecentErr {
				loadLogs = append(loadLogs, fmt.Sprintf("%v エラーが発生したため負荷レベルを上げられませんでした。%v", now, e))
				log.Println("Cannot increase Load Level. Reason: RecentErr", e, "Before", time.Since(et))
			} else if hasRecentSlowPath {
				loadLogs = append(loadLogs, fmt.Sprintf("%v レスポンスが遅いため負荷レベルを上げられませんでした。%v", now, path))
				log.Println("Cannot increase Load Level. Reason: SlowPath", path, "Before", time.Since(st))
			} else {
				loadLogs = append(loadLogs, fmt.Sprintf("%v 負荷レベルが上昇しました。", now))
				counter.IncKey("load-level-up")
				log.Println("Increase Load Level.")
				levelUpLoad(ctx, state, 5)
			}
		case <-ctx.Done():
			// ベンチ終了、このタイミングでエラーの収集をやめる。
			bench.GuardCheckerError(true)
			return
		}
	}
}

func printCounterSummary() {
	m := map[string]int64{}
	// TODO(sonots): Fix
	for key, count := range counter.GetMap() {
		if strings.HasPrefix(key, "GET|/history/") {
			key = "GET|/history/*"
		} else if strings.HasPrefix(key, "GET|/message?") {
			key = "GET|/message?*"
		} else if strings.HasPrefix(key, "GET|/icons/") {
			key = "GET|/icons/*"
		} else if strings.HasPrefix(key, "GET|/channel/") {
			key = "GET|/channel/*"
		} else if strings.HasPrefix(key, "GET|/profile/") {
			key = "GET|/profile/*"
		}

		if strings.HasPrefix(key, "SKIP|/icons/") {
			key = "SKIP|/icons/*"
		}

		m[key] += count
	}

	type p struct {
		Key   string
		Value int64
	}
	var s []p

	for key, count := range m {
		s = append(s, p{key, count})
	}

	sort.Slice(s, func(i, j int) bool { return s[i].Value > s[j].Value })

	log.Println("----- Request counts -----")
	for _, kv := range s {
		if strings.HasPrefix(kv.Key, "GET|") || strings.HasPrefix(kv.Key, "POST|") {
			log.Println(kv.Key, kv.Value)
		}
	}
	log.Println("----- Other counts ------")
	for _, kv := range s {
		if strings.HasPrefix(kv.Key, "GET|") || strings.HasPrefix(kv.Key, "POST|") {
		} else {
			log.Println(kv.Key, kv.Value)
		}
	}
	log.Println("-------------------------")
}

func startBenchmark(remoteAddrs []string) *BenchResult {
	addLoadFunc(1, benchFunc{"LoadCreateUser", bench.LoadCreateUser})
	addLoadFunc(1, benchFunc{"loadLogin", bench.LoadLogin})

	addLoadLevelUpFunc(1, benchFunc{"loadTopPage", bench.LoadTopPage})

	addCheckFunc(benchFunc{"CheckStaticFiles", bench.CheckStaticFiles})
	addCheckFunc(benchFunc{"CheckCreateUser", bench.CheckCreateUser})
	addCheckFunc(benchFunc{"CheckLogin", bench.CheckLogin})
	addCheckFunc(benchFunc{"CheckAdminLogin", bench.CheckAdminLogin})
	addCheckFunc(benchFunc{"CheckAdminCreateEvent", bench.CheckAdminCreateEvent})

	result := new(BenchResult)
	result.StartTime = time.Now()
	defer func() {
		result.EndTime = time.Now()
	}()

	getErrorsString := func() []string {
		var errors []string
		for _, err := range bench.GetCheckerErrors() {
			errors = append(errors, err.Error())
		}
		return errors
	}

	state := new(bench.State)

	log.Println("State.Init()")
	state.Init()
	log.Println("State.Init() Done")

	log.Println("requestInitialize()")
	err := requestInitialize(bench.GetRandomTargetHost())
	if err != nil {
		result.Score = 0
		result.Errors = getErrorsString()
		result.Message = fmt.Sprint("/initialize へのリクエストに失敗しました。", err)
		return result
	}
	log.Println("requestInitialize() Done")

	ctx, cancel := context.WithTimeout(context.Background(), benchDuration)
	defer cancel()

	log.Println("preTest()")
	err = preTest(ctx, state)
	if err != nil {
		result.Score = 0
		result.Errors = getErrorsString()
		result.Message = fmt.Sprint("負荷走行前のバリデーションに失敗しました。", err)
		return result
	}
	log.Println("preTest() Done")

	if preTestOnly {
		result.Score = 0
		result.Errors = getErrorsString()
		result.Message = fmt.Sprint("preTest passed.")
		return result
	}

	log.Println("validationMain()")
	go benchmarkMain(ctx, state)
	for {
		err = validationMain(ctx, state)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			result.Score = 0
			result.Errors = getErrorsString()
			result.Message = fmt.Sprint("負荷走行中のバリデーションに失敗しました。", err)
			return result
		}
	}
	log.Println("validationMain() Done")

	printCounterSummary()

	// TODO(sonots): Fix
	getCount := counter.SumPrefix(`GET|/`)
	fetchCount := counter.SumPrefix(`GET|/fetch`)
	postCount := counter.SumPrefix(`POST|/`)
	msgCount := counter.SumPrefix(`get-message-count`)
	s304Count := counter.GetKey("staticfile-304")
	score := 1*(getCount-fetchCount-s304Count) + 3*postCount + 1*msgCount + s304Count/100

	log.Println("get", getCount)
	log.Println("fetch", fetchCount)
	log.Println("post", postCount)
	log.Println("msg", msgCount)
	log.Println("s304", s304Count)
	log.Println("score", score)

	result.LoadLevel = int(counter.GetKey("load-level-up"))
	result.Pass = true
	result.Score = score
	result.Errors = getErrorsString()
	result.Message = "ok"
	return result
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.SetPrefix("[isu8q-bench] ")

	var (
		workermode bool
		portalUrl  string
		dataPath   string
		remotes    string
		output     string
		jobid      string
		tempdir    string
		test       bool
		debug      bool
		nolevelup  bool
		duration   time.Duration
	)

	flag.BoolVar(&workermode, "workermode", false, "workermode")
	flag.StringVar(&portalUrl, "portal", "http://localhost:8888", "portal site url (only used at workermode)")
	flag.StringVar(&dataPath, "data", "./data", "path to data directory")
	flag.StringVar(&remotes, "remotes", "localhost:8080", "remote addrs to benchmark")
	flag.StringVar(&output, "output", "", "path to write result json")
	flag.StringVar(&jobid, "jobid", "", "job id")
	flag.StringVar(&tempdir, "tempdir", "", "path to temp dir")
	flag.BoolVar(&test, "test", false, "run pretest only")
	flag.BoolVar(&debug, "debug", false, "add debugging info into request header")
	flag.DurationVar(&duration, "duration", time.Minute, "benchamrk duration")
	flag.BoolVar(&nolevelup, "nolevelup", false, "dont increase load level")
	flag.Parse()

	bench.DebugMode = debug
	bench.DataPath = dataPath
	bench.PrepareDataSet()

	preTestOnly = test
	noLevelup = nolevelup
	benchDuration = duration

	if workermode {
		runWorkerMode(tempdir, portalUrl)
		return
	}

	go func() {
		log.Println(http.ListenAndServe(fmt.Sprintf(":%d", pprofPort), nil))
	}()

	remoteAddrs := strings.Split(remotes, ",")
	if 0 == len(remoteAddrs) {
		log.Fatalln("invalid remotes")
	}
	log.Println("Remotes", remoteAddrs)

	bench.SetTargetHosts(remoteAddrs)

	result := startBenchmark(remoteAddrs)
	result.IPAddrs = remotes
	result.JobID = jobid
	result.Logs = loadLogs

	b, err := json.Marshal(result)
	if err != nil {
		log.Fatalln(err)
	}

	log.Println(string(b))

	if output != "" {
		err := ioutil.WriteFile(output, b, 0644)
		if err != nil {
			log.Fatalln(err)
		}
		log.Println("result json saved to ", output)
	}
}
