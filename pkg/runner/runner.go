package runner

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/panjf2000/ants"
	"github.com/zan8in/afrog/pkg/catalog"
	"github.com/zan8in/afrog/pkg/config"
	"github.com/zan8in/afrog/pkg/core"
	"github.com/zan8in/afrog/pkg/fingerprint"
	"github.com/zan8in/afrog/pkg/html"
	"github.com/zan8in/afrog/pkg/log"
	"github.com/zan8in/afrog/pkg/output"
	"github.com/zan8in/afrog/pkg/poc"
	"github.com/zan8in/afrog/pkg/protocols/http/retryhttpclient"
	"github.com/zan8in/afrog/pkg/targetlive"
	"github.com/zan8in/afrog/pkg/upgrade"
	"github.com/zan8in/afrog/pkg/utils"
	"github.com/zan8in/gologger"
)

type Runner struct {
	options *config.Options
	catalog *catalog.Catalog

	ChanTargets    chan string
	ChanBadTargets chan string
	ChanPocs       chan poc.Poc

	targetsTemp string

	ticker *time.Ticker
}

func New(options *config.Options, htemplate *html.HtmlTemplate, acb config.ApiCallBack) error {
	runner := &Runner{
		options:        options,
		ChanTargets:    make(chan string),
		ChanBadTargets: make(chan string),
		ChanPocs:       make(chan poc.Poc),
	}

	// afrog engine update
	if options.UpdateAfrogVersion {
		return UpdateAfrogVersionToLatest(true)
	}

	// print pocs list
	if options.PrintPocs {
		options.PrintPocList()
		return nil
	}

	// update afrog-pocs
	upgrade := upgrade.New(options.UpdatePocs)
	upgrade.UpgradeAfrogPocs()
	if options.UpdatePocs {
		return nil
	}

	// output to afrog report
	if len(options.Output) == 0 {
		options.Output = utils.GetNowDateTimeReportName() + ".html"
	}
	htemplate.Filename = options.Output
	if err := htemplate.New(); err != nil {
		gologger.Fatal().Msgf("Output failed, %s", err.Error())
	}

	// output to json file
	if len(options.OutputJson) > 0 {
		options.OJ = output.NewOutputJson(options.OutputJson)
	}

	// show banner
	ShowBanner2(upgrade)

	// init TargetLive
	targetlive.New(options.MaxHostError)

	// init callback
	options.ApiCallBack = acb

	// init proxyURL
	if err := config.LoadProxyServers(options); err != nil {
		return err
	}

	// init config file
	config, err := config.New()
	if err != nil {
		return err
	}
	options.Config = config

	// init rtryhttp
	retryhttpclient.Init(options)

	// init targets
	go func() {
		if err := runner.PreprocessTargets(); err != nil {
			gologger.Error().Msg(err.Error())
		}
	}()

	// init pocs
	go func() {
		if err := runner.PreprocessPocs(); err != nil {
			gologger.Error().Msg(err.Error())
		}
	}()

	fmt.Println()

	// show banner
	// gologger.Info().Msgf("PoCs added in last update: %d", len(allPocsYamlSlice))
	// gologger.Info().Msgf("PoCs loaded for scan: %d", len(allPocsYamlSlice)+len(allPocsEmbedYamlSlice))
	// gologger.Info().Msgf("Creating output html file: %s", htemplate.Filename)

	// reverse set
	if len(options.Config.Reverse.Ceye.Domain) == 0 || len(options.Config.Reverse.Ceye.ApiKey) == 0 {
		homeDir, _ := os.UserHomeDir()
		configDir := homeDir + "/.config/afrog/afrog-config.yaml"
		gologger.Error().Msgf("`ceye` reverse service not set: %s", configDir)
	}

	// fingerprint
	if !options.NoFinger {
		s, _ := fingerprint.New(options)
		s.Execute()
		if len(s.ResultSlice) > 0 {
			htemplate.AppendFinger(s.ResultSlice)
			printFingerResultConsole()
		}
	}

	if !options.OnlyFinger {
		//check target live
		go runTargetLivenessCheck(options)

		// check poc
		e := core.New(options)
		runner.ticker = time.NewTicker(time.Second / time.Duration(options.RateLimit))
		var wg sync.WaitGroup

		p, _ := ants.NewPoolWithFunc(options.RateLimit, func(p any) {
			defer wg.Done()
			<-runner.ticker.C

			tap := p.(*core.TargetAndPocs)

			if len(tap.Target) > 0 && len(tap.Poc.Id) > 0 {
				ctx := context.Background()
				e.ExecuteExpression(ctx, tap.Target, &tap.Poc)
			}

		})
		defer p.Release()

		for poc := range runner.ChanPocs {
			for t := range runner.ChanTargets {
				wg.Add(1)
				p.Invoke(&core.TargetAndPocs{Target: t, Poc: poc})
			}
		}

		wg.Wait()

	}

	return nil
}

func printFingerResultConsole() {
	gologger.Print().Msgf("\r" + log.LogColor.Time("000 "+utils.GetNowDateTime()) + " " +
		log.LogColor.Vulner("Fingerprint") + " " + log.LogColor.Info("INFO") + "                    \r\n")

}

func runTargetLivenessCheck(options *config.Options) {
	first := true
	m := sync.Mutex{}
	// for i := 0; i < options.MaxHostError; i++ {
	for {
		// fmt.Println("\r\nRunTargetLivenessCheck start", len(options.Targets), options.TargetLive.GetNoLiveAtomicCount())
		// reqCount := 0
		if len(options.Targets) > 0 {

			var wg sync.WaitGroup

			p, _ := ants.NewPoolWithFunc(options.FingerprintConcurrency, func(wgTask any) {
				defer wg.Done()

				url := wgTask.(poc.WaitGroupTask).Value.(string)
				key := wgTask.(poc.WaitGroupTask).Key
				statusCode := 0

				// 首次检测 target list
				if first && targetlive.TLive.HandleTargetLive(url, -1) != -1 {

					// url, statusCode := http2.CheckTargetHttps(url)
					url, statusCode = retryhttpclient.CheckHttpsAndLives(url)

					if statusCode == -1 || statusCode >= http.StatusInternalServerError {
						if statusCode == -1 && !utils.IsURL(url) {
							url = "http://" + url
						}
						targetlive.TLive.HandleTargetLive(url, 0)
					} else {
						targetlive.TLive.HandleTargetLive(url, 1)
					}
					// reqCount += 1
				}

				// 非首次检测 target list
				if !first && targetlive.TLive.HandleTargetLive(url, -1) == 2 {

					// url, statusCode := http2.CheckTargetHttps(url)
					url, statusCode = retryhttpclient.CheckHttpsAndLives(url)

					if statusCode == -1 || statusCode >= http.StatusInternalServerError {
						if statusCode == -1 && !utils.IsURL(url) {
							url = "http://" + url
						}
						targetlive.TLive.HandleTargetLive(url, 0)
					} else {
						targetlive.TLive.HandleTargetLive(url, 1)
					}
					// reqCount += 1
				}

				if !utils.IsURL(url) {
					m.Lock()
					options.Targets[key] = url
					m.Unlock()
				}

			})
			defer p.Release()

			for k, target := range options.Targets {
				// if !utils.IsURL(target) {
				// 	gologger.Print().Msgf("[CheckLive] %s", target)
				// }
				wg.Add(1)
				_ = p.Invoke(poc.WaitGroupTask{Value: target, Key: k})
			}
			wg.Wait()
		}
		// fmt.Println("request count", reqCount)
		// reqCount = 0
		first = false
		time.Sleep(time.Second * 10)
		// fmt.Println("target noLive count: ", options.TargetLive.GetNoLiveCount(), options.TargetLive.GetNoLiveAtomicCount())
		// for _, target := range options.TargetLive.GetNoLiveSlice() {
		// 	fmt.Println("\r\nnolive target: ", target)
		// }
		// lt := options.TargetLive.ListRequestTargets()
		// if len(lt) > 0 {
		// 	for _, v := range lt {
		// 		fmt.Println("777777777777777", v, " 仍在请求中...")
		// 	}
		// }
		// fmt.Println("正在请求中....总数：", len(lt))
	}

}
