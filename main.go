package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c-ollins/crabada/idlegame"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	tb "gopkg.in/tucnak/telebot.v2"
)

const (
	InternalServerError = "INTERNAL_SERVER_ERROR"

	IdleContractAddress = "0x82a85407BD612f52577909F4A58bfC6873f14DA8"

	TelegramChatID = 0

	TeamAvailable = "AVAILABLE"

	PollURL  = "https://idle-api.crabada.com/public/idle/mines?page=1&status=open&looter_address=0xed3428bcc71d3b0a43bb50a64ed774bec57100a8&can_loot=1&limit=8"
	LootURL  = "https://idle-api.crabada.com/public/idle/mines?looter_address=%s&page=1&status=open&limit=8"
	TeamsURL = "https://idle-api.crabada.com/public/idle/teams?user_address=%s"

	GasAPI = "https://api.debank.com/chain/gas_price_dict_v2?chain=avax"
)

var (
	TelegramChat   = &tb.Chat{ID: TelegramChatID}
	MsgSendOptions = &tb.SendOptions{DisableWebPagePreview: true}

	settleRegex = regexp.MustCompile(`\/settle (.+)`)
	attackRegex = regexp.MustCompile(`\/attack (.+)`)

	actionReinforceDefense = "reinforce-defense"
	actionAttack           = "attack"

	processIntervals = 30 * time.Minute

	wallets = []string{"0xed3428BcC71d3B0a43Bb50a64ed774bEc57100a8", "0xf91fF01b9EF0d83D0bBd89953d53504f099A3DFf"}
)

func main() {
	et := etubot{
		isAuto:     true,
		attackCh:   make(chan *Team, 5),
		privateKey: make(map[string]*ecdsa.PrivateKey),
	}
	et.start()
}

type etubot struct {
	bot    *tb.Bot
	isAuto bool

	gasPrice *big.Int
	gasMu    sync.RWMutex

	attackCh chan *Team

	client       *ethclient.Client
	idleContract *idlegame.Idlegame
	privateKey   map[string]*ecdsa.PrivateKey
}

func (et *etubot) start() {

	initLogRotator("logs.txt")

	privateKey, err := crypto.HexToECDSA(os.Getenv("BOT_PRIVATE"))
	if err != nil {
		log.Error(err)
		return
	}
	et.privateKey[strings.ToLower("0xed3428BcC71d3B0a43Bb50a64ed774bEc57100a8")] = privateKey

	privateKey2, err := crypto.HexToECDSA(os.Getenv("BOT_PRIVATE2"))
	if err != nil {
		log.Error(err)
		return
	}
	et.privateKey[strings.ToLower("0xf91fF01b9EF0d83D0bBd89953d53504f099A3DFf")] = privateKey2

	log.Info("Connecting to infura")
	client, err := ethclient.Dial("https://api.avax.network/ext/bc/C/rpc")
	if err != nil {
		log.Error("ethclient:", err)
		return
	}
	et.client = client

	log.Info("Connected to infura")

	address := common.HexToAddress(IdleContractAddress)
	idleContract, err := idlegame.NewIdlegame(address, client)
	if err != nil {
		log.Error(err)
		return
	}

	et.idleContract = idleContract

	b, err := tb.NewBot(tb.Settings{
		Token:  "",
		Poller: &tb.LongPoller{Timeout: 10 * time.Second},
	})
	if err != nil {
		log.Error(err)
		return
	}
	et.bot = b

	b.Handle(tb.OnText, func(m *tb.Message) {
		switch {
		case m.Text == "/ping":
			b.Reply(m, "pong!")
			return
		case m.Text == "/gas":
			go et.gas(m)
			return
		case m.Text == "/raid":
			if et.isAuto {
				et.bot.Send(TelegramChat, "cmd disabled in auto.")
				return
			}
			go et.raid()
			return
		case m.Text == "/loots":
			go et.sendActiveLoots(m)
			return
		case m.Text == "/settleall":
			if et.isAuto {
				et.bot.Send(TelegramChat, "cmd disabled in auto.")
				return
			}
			go et.settleAll(false)
			return
		case m.Text == "/teams":
			go et.sendTeams(m)
			return
		}

		if matches := settleRegex.FindStringSubmatch(m.Text); len(matches) > 1 {
			if et.isAuto {
				et.bot.Send(TelegramChat, "cmd disabled in auto.")
				return
			}

			gameID, err := strconv.Atoi(matches[1])
			if err != nil {
				b.Reply(m, err.Error())
				return
			}

			go et.settleGame(int64(gameID))
		} else if matches := attackRegex.FindStringSubmatch(m.Text); len(matches) > 1 {
			if et.isAuto {
				et.bot.Send(TelegramChat, "cmd disabled in auto.")
				return
			}

			teamID, err := strconv.Atoi(matches[1])
			if err != nil {
				b.Reply(m, err.Error())
				return
			}

			go func() {
				team, err := et.teamForID(int64(teamID))
				if err != nil {
					if err != nil {
						b.Reply(m, fmt.Sprintf("Could not find team:%v", err))
						return
					}
				}

				et.attackCh <- team
				b.Reply(m, fmt.Sprintf("Attacking using team #%d", team.ID))
			}()
		}
	})

	log.Info("Bot running")
	err = et.updateGasPrice()
	if err != nil {
		log.Error(err)
		return
	}

	// go et.watchStartGame()
	// go et.queAttacks()
	if et.isAuto {
		// go et.auto()
	}
	go et.gasUpdate()
	b.Start()

	select {}
}

func (et *etubot) raid() {
	teams, err := et.allTeams()
	if err != nil {
		et.sendError(fmt.Errorf("error finding teams team:%v", err))
		return
	}

	queue := 0
	for _, team := range teams {

		if !et.teamIsAvailable(team.ID) {
			// lg := fmt.Sprintf("Cannot attack, team %d is not available", team.ID)
			// log.Info(lg)
			// et.bot.Send(TelegramChat, lg)
			continue
		}

		et.pollGamesAndAttack(team)
		queue++
	}

	if !et.isAuto && queue == 0 {
		et.bot.Send(TelegramChat, "All teams are busy.")
	}
}

func (et *etubot) queAttacks() {
	log.Info("Attacks queue active")
	for team := range et.attackCh {

		if !et.teamIsAvailable(team.ID) {
			lg := fmt.Sprintf("Cannot attack, team %d is not available", team.ID)
			log.Info(lg)
			et.bot.Send(TelegramChat, lg)
			continue
		}

		et.pollGamesAndAttack(team)
	}
}

func (et *etubot) pollGamesAndAttack(team *Team) {
	errorCount := 0
	lastBlock := uint64(0)
	et.bot.Send(TelegramChat, fmt.Sprintf("Finding game using team #%d", team.ID))

	for {
		bestBlock, err := et.client.BlockNumber(context.Background())
		if err != nil {
			et.sendError(fmt.Errorf("error getting block number: %v", err))
			continue
		}

		if bestBlock == lastBlock {
			continue
		}

		filterOpts := &bind.FilterOpts{Context: context.Background(), Start: bestBlock}
		gamesIter, err := et.idleContract.FilterStartGame(filterOpts)
		if err != nil {
			et.sendError(fmt.Errorf("error filtering start game: %v", err))
			continue
		}

		var target *Game
		for gamesIter.Next() {
			gameInfo, err := et.idleContract.GetGameBasicInfo(&bind.CallOpts{Context: context.Background()}, gamesIter.Event.GameId)
			if err != nil {
				et.sendError(fmt.Errorf("error getting game info: %v", err))
				continue
			}

			teamInfo, err := et.idleContract.GetTeamInfo(&bind.CallOpts{Context: context.Background()}, gamesIter.Event.TeamId)
			if err != nil {
				et.sendError(fmt.Errorf("error getting team info: %v", err))
				continue
			}

			gameAge := time.Since(time.Unix(int64(gameInfo.StartTime), 0))
			strengthDiff := team.Strength - int(teamInfo.BattlePoint)
			if strengthDiff >= 20 && gameAge < (3*time.Second) {
				target = &Game{ID: gamesIter.Event.GameId.Int64(), DefensePoint: int(teamInfo.BattlePoint), StartTime: int64(gameInfo.StartTime)}
				break
			}
		}

		if target == nil {
			continue
		}

		log.Infof("Game: %d, opponent strength: %d, team strength: %d, start time:%s", target.ID, target.DefensePoint, team.Strength, time.Since(time.Unix(target.StartTime, 0)).Truncate(time.Second))

		err = et.attack(target, team)
		if err != nil {
			errorCount++
			if errorCount >= 3 {
				et.bot.Send(TelegramChat, fmt.Sprintf("More than 3 errors while trying to attack using team %d. %v", team.ID, err))
				return
			}

			log.Error("error attacking:", err)
			continue
		}

		break
	}
}

func (et *etubot) attack(game *Game, team *Team) error {

	strengthDiff := team.Strength - game.DefensePoint
	lg := fmt.Sprintf("Attacking %d using %d, strength advantage: %d.", game.ID, team.ID, strengthDiff)
	log.Info(lg)
	auth, err := et.txAuth(team.Wallet)
	if err != nil {
		return err
	}

	tx, err := et.idleContract.Attack(auth, big.NewInt(game.ID), big.NewInt(team.ID))
	if err != nil {
		return err
	}

	log.Info("Attack tx hash:", tx.Hash())
	// wait for receipt
	waitStart := time.Now()
	for {
		receipt, err := et.client.TransactionReceipt(context.Background(), tx.Hash())
		if err != nil {
			if err != ethereum.NotFound {
				log.Error("error:", err)
			}
			time.Sleep(5 * time.Second)
			if time.Since(waitStart) > 2*time.Minute {
				return fmt.Errorf("transaction failed on: %d, did not get receipt after 2 minutes", game.ID)
			}
			continue
		}

		log.Info(receipt)
		if receipt.Status == types.ReceiptStatusSuccessful {
			et.bot.Send(TelegramChat, fmt.Sprintf("Game #%d attack successful by team #%d, defense adv: %d.\nhttps://snowtrace.io/tx/%s", game.ID, team.ID, strengthDiff, tx.Hash().String()), MsgSendOptions)
			return nil
		}

		log.Info("Attack failed, retrying")
		return fmt.Errorf("transaction failed on: %d", game.ID)
	}
}

func (et *etubot) sendError(err error) {
	log.Error(err)
	// et.bot.Send(TelegramChat, err.Error())
}

func (et *etubot) settleAll(isAuto bool) {
	games, err := et.activeLoots()
	if err != nil {
		et.sendError(fmt.Errorf("error fetching active loots: %v", err))
		return
	}

	totalSettled := 0
	for _, game := range games {
		if game.canSettle() {
			totalSettled++
			et.settleGame(game.ID)
		}
	}

	if totalSettled == 0 && !isAuto {
		et.bot.Send(TelegramChat, "No games ready to be settled.")
	}
}

func (et *etubot) settleGame(gameID int64) {
	log.Info("Settling game", gameID)
	et.bot.Send(TelegramChat, fmt.Sprintf("Settling game #%d", gameID))
	team, err := et.findMyLootTeam(gameID)
	if err != nil {
		et.sendError(fmt.Errorf("error finding loot team:%v", err))
		return
	}

	auth, err := et.txAuth(team.Wallet)
	if err != nil {
		et.sendError(fmt.Errorf("error creating tx auth: %v", err))
		return
	}

	tx, err := et.idleContract.SettleGame(auth, big.NewInt(gameID))
	if err != nil {
		et.sendError(fmt.Errorf("error sending settle tx: %v", err))
		return
	}

	log.Info("Settle hash:", tx.Hash())
	// wait for receipt
	waitStart := time.Now()
	for {
		log.Info("Checking for receipt")
		_, err := et.client.TransactionReceipt(context.Background(), tx.Hash())
		if err != nil {
			if err != ethereum.NotFound {
				log.Error("error:", err)
			}
			if time.Since(waitStart) > 2*time.Minute {
				et.bot.Send(TelegramChat, fmt.Sprintf("Game #%d Team #%d has been settled, but didnt not get confirmed in 1 minute.\nhttps://snowtrace.io/tx/%s", gameID, team.ID, tx.Hash().String()), MsgSendOptions)
				return
			}
			time.Sleep(5 * time.Second)
			continue
		}
		et.bot.Send(TelegramChat, fmt.Sprintf("Game #%d Team #%d has been settled.\nhttps://snowtrace.io/tx/%s", gameID, team.ID, tx.Hash().String()), MsgSendOptions)
		break
	}
}

func (et *etubot) findMyLootTeam(gameID int64) (*Team, error) {
	var response GameResponse
	err := makeRequest(fmt.Sprintf("https://idle-api.crabada.com/public/idle/mine/%d", gameID), &response)
	if err != nil {
		return nil, fmt.Errorf("error fetching game by id: %v", err)
	}

	if response.ErrorCode != "" {
		return nil, fmt.Errorf("error fetching game by id: %s, message: %s", response.ErrorCode, response.Message)
	}

	// TODO: verify team

	return &Team{ID: response.AttackTeamID, Strength: response.AttackPoint, Wallet: response.AttackTeamOwner}, nil
}

func (et *etubot) txAuth(address string) (*bind.TransactOpts, error) {
	nonce, err := et.client.PendingNonceAt(context.Background(), common.HexToAddress(address))
	if err != nil {
		return nil, fmt.Errorf("error getting nonce: %v", err)
	}

	// gasPrice, err := et.client.SuggestGasPrice(context.Background())
	// if err != nil {
	// 	return nil, err
	// }

	auth, err := bind.NewKeyedTransactorWithChainID(et.privateKey[strings.ToLower(address)], big.NewInt(43114))
	if err != nil {
		return nil, fmt.Errorf("error creating transactor: %v", err)
	}

	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)     // in wei
	auth.GasLimit = uint64(200000) // in units // TODO
	// auth.GasPrice = big.NewInt(0).Add(gasPrice, big.NewInt(50000000000)) // add 30 gwei
	et.gasMu.RLock()
	auth.GasPrice = big.NewInt(0).Add(et.gasPrice, big.NewInt(30000000000))
	et.gasMu.RUnlock()
	limit := big.NewInt(200000000000) //200gwei

	if auth.GasPrice.Cmp(limit) >= 0 {
		// et.bot.Send(TelegramChat, "Cannot make transaction, gas is higher than 200gwei.")
		return nil, fmt.Errorf("cannot make tx, gas too high")
	}

	log.Info("Using gas:", big.NewInt(0).Div(auth.GasPrice, big.NewInt(1e9)), auth.GasPrice.Cmp(limit))

	return auth, nil
}

func (et *etubot) activeLoots() ([]Game, error) {
	var games []Game
	for i := 0; i < len(wallets); i++ {

		var response GamesResponse
		err := makeRequest(fmt.Sprintf(LootURL, wallets[i]), &response)
		if err != nil {
			log.Error("Error fetching active loots:", err)
			return nil, err
		}

		if response.ErrorCode != "" {
			return nil, fmt.Errorf("error fetching game by id: %s, message: %s", response.ErrorCode, response.Message)
		}

		games = append(games, response.Games...)
	}

	return games, nil
}

func (et *etubot) sendActiveLoots(msg *tb.Message) {

	games, err := et.activeLoots()
	if err != nil {
		et.sendError(fmt.Errorf("error fetching active loots: %v", err))
		return
	}
	sb := fmt.Sprintf("%d active loots 💰🤑💰\n-------------------------\n", len(games))
	for _, game := range games {
		lootSummary := "💰 Loot\n"
		lootSummary += fmt.Sprintf("Game: %d\n", game.ID)
		lootSummary += fmt.Sprintf("Team: %d\n", game.AttackTeamID)
		lootSummary += fmt.Sprintf("Account: %s\n", game.AttackTeamOwner[:7])
		if len(game.Process) > 0 {
			latestProcess := game.lastProcess()
			var lastAction, status string
			if latestProcess.Action == actionAttack {
				lastAction = "attacked"
				if time.Since(latestProcess.txTime()) > processIntervals {
					status = "won"
				} else {
					status = "waiting for opponent's reinforcement"
				}
			} else if latestProcess.Action == actionReinforceDefense {
				lastAction = "opponent reinforced"
				if time.Since(latestProcess.txTime()) > processIntervals {
					status = "lost"
				} else {
					status = fmt.Sprintf("%d reinforcement needed", game.DefensePoint-game.AttackPoint)
				}
			}
			lootSummary += fmt.Sprintf("Last action: %s\n", lastAction)
			lootSummary += fmt.Sprintf("Status: %s\n", status)
		}
		if game.canSettle() {
			lootSummary += "Ready: yes\n"
		} else {
			lootSummary += fmt.Sprintf("Settle in: %s\n", time.Until(game.settleTime()))
		}

		sb += "\n" + lootSummary
	}

	et.bot.Reply(msg, sb)
}

func (et *etubot) allTeams() ([]*Team, error) {
	var teams []*Team
	for i := 0; i < len(wallets); i++ {
		var response TeamsResponse
		err := makeRequest(fmt.Sprintf(TeamsURL, wallets[i]), &response)
		if err != nil {
			log.Error("Error fetching teams:", err)
			return nil, err
		}

		if response.ErrorCode != "" {
			return nil, fmt.Errorf("error fetching game by id: %s, message: %s", response.ErrorCode, response.Message)
		}

		if response.TotalRecord > 0 {
			teams = append(teams, response.Teams...)
		}
	}

	return teams, nil
}

func (et *etubot) teamForID(teamID int64) (*Team, error) {
	teams, err := et.allTeams()
	if err != nil {
		return nil, err
	}

	for _, team := range teams {
		if team.ID == teamID {
			return team, nil
		}
	}

	return nil, fmt.Errorf("team does not exist in wallets")
}

func (et *etubot) teamIsAvailable(teamID int64) bool {
	team, err := et.teamForID(teamID)
	if err != nil {
		et.sendError(fmt.Errorf("error fetching teams: %v", err))
		return false
	}

	if team.Status == "AVAILABLE" {
		return true
	}

	return false
}

func (et *etubot) sendTeams(msg *tb.Message) {

	teams, err := et.allTeams()
	if err != nil {
		et.sendError(fmt.Errorf("error fetching teams: %v", err))
		return
	}

	sb := fmt.Sprintf("%d teams\n---------------------\n", len(teams))

	for _, team := range teams {
		teamSummary := fmt.Sprintf("ID: %d\n", team.ID)
		teamSummary += fmt.Sprintf("Strength: %d\n", team.Strength)
		teamSummary += fmt.Sprintf("Account: %s\n", team.Wallet[:7])
		teamSummary += fmt.Sprintf("Status: %s\n", strings.ToLower(team.Status))

		sb += "\n" + teamSummary
	}

	et.bot.Reply(msg, sb)
}

func (et *etubot) gas(msg *tb.Message) {

	et.gasMu.RLock()
	gasPrice := et.gasPrice
	et.gasMu.RUnlock()

	et.bot.Reply(msg, fmt.Sprintf("%d gwei", big.NewInt(0).Div(gasPrice, big.NewInt(1e9))))
}

func (et *etubot) gasUpdate() {
	fmt.Println("Gas update running")
	for {
		err := et.updateGasPrice()
		if err != nil {
			et.sendError(fmt.Errorf("err updating gas: %v", err))
		}
		time.Sleep(10 * time.Second)
	}
}

func (et *etubot) updateGasPrice() error {
	var response GasResponse
	err := makeRequest(GasAPI, &response)
	if err != nil {
		return fmt.Errorf("error fetching gas: %v", err)
	}

	// fmt.Printf("New gas price: %d\n", int64(response.Data.Fast.Price))

	gasPrice := big.NewInt(int64(response.Data.Fast.Price))
	et.gasMu.Lock()
	et.gasPrice = gasPrice
	et.gasMu.Unlock()

	return nil
}

func (et *etubot) watchStartGame() {
	for {
		bestBlock, err := et.client.BlockNumber(context.Background())
		if err != nil {
			et.sendError(fmt.Errorf("error getting block number: %v", err))
			return
		}

		log.Info("Bestblock:", bestBlock)
		filterOpts := &bind.FilterOpts{Context: context.Background(), Start: bestBlock}
		gamesIter, err := et.idleContract.FilterStartGame(filterOpts)
		if err != nil {
			et.sendError(fmt.Errorf("error filtering start game: %v", err))
			return
		}

		for gamesIter.Next() {
			gameInfo, err := et.idleContract.GetGameBasicInfo(&bind.CallOpts{Context: context.Background()}, gamesIter.Event.GameId)
			if err != nil {
				et.sendError(fmt.Errorf("error getting game info: %v", err))
				return
			}

			teamInfo, err := et.idleContract.GetTeamInfo(&bind.CallOpts{Context: context.Background()}, gamesIter.Event.TeamId)
			if err != nil {
				et.sendError(fmt.Errorf("error getting game info: %v", err))
				return
			}

			gameAge := time.Since(time.Unix(int64(gameInfo.StartTime), 0)).Truncate(time.Second)

			log.Info("Game:", gamesIter.Event.GameId, "Strength:", teamInfo.BattlePoint, "Start:", gameAge)
		}
	}
	// watchOpts := &bind.WatchOpts{Context: context.Background(), Start: nil}
	// channel := make(chan *idlegame.IdlegameStartGame)

	// // Start a goroutine which watches new events
	// go func() {
	// 	log.Info("Subscribing to contract")
	// 	sub, err := et.idleContract.WatchStartGame(watchOpts, channel)
	// 	if err != nil {
	// 		et.sendError(fmt.Errorf("error watching start game: %v", err))
	// 		return
	// 	}
	// 	defer sub.Unsubscribe()
	// }()

	// log.Info("Watch start game")

	// for {
	// 	event := <-channel
	// 	log.Infof("New game id: %d, team id: %d", event.GameId, event.TeamId)
	// }
}

func (et *etubot) auto() {
	et.bot.Send(TelegramChat, "Etubot running on auto.")
	for {
		log.Info("Auto running")
		// settle ready games

		et.gasMu.RLock()
		gasPrice := big.NewInt(0).Add(et.gasPrice, big.NewInt(30000000000))
		et.gasMu.RUnlock()
		limit := big.NewInt(200000000000) //200gwei

		if gasPrice.Cmp(limit) >= 0 {
			// notify high gas
			// continue
		} else {
			et.settleAll(true)
			et.raid()
		}

		time.Sleep(30 * time.Second)
	}
}
