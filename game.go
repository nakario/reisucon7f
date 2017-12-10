package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"
	"sync"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"github.com/jmoiron/sqlx"
	"golang.org/x/sync/singleflight"
	"math"
)

const duration = 700 * time.Millisecond
const cacheSize = 100000
var group singleflight.Group
var rooms sync.Map
var (
	big1000 = big.NewInt(1000)
)

type Room struct {
	wg *sync.WaitGroup
	c *sync.Cond
}

type GameRequest struct {
	RequestID int    `json:"request_id"`
	Action    string `json:"action"`
	Time      int64  `json:"time"`

	// for addIsu
	Isu string `json:"isu"`

	// for buyItem
	ItemID      int `json:"item_id"`
	CountBought int `json:"count_bought"`
}

type GameResponse struct {
	RequestID int  `json:"request_id"`
	IsSuccess bool `json:"is_success"`
}

// 10進数の指数表記に使うデータ。JSONでは [仮数部, 指数部] という2要素配列になる。
type Exponential struct {
	// Mantissa * 10 ^ Exponent
	Mantissa int64
	Exponent int64
}

func (n Exponential) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("[%d,%d]", n.Mantissa, n.Exponent)), nil
}

type Adding struct {
	RoomName string `json:"-" db:"room_name"`
	Time     int64  `json:"time" db:"time"`
	Isu      string `json:"isu" db:"isu"`
}

type Buying struct {
	RoomName string `db:"room_name"`
	ItemID   int    `db:"item_id"`
	Ordinal  int    `db:"ordinal"`
	Time     int64  `db:"time"`
}

type Schedule struct {
	Time       int64       `json:"time"`
	MilliIsu   Exponential `json:"milli_isu"`
	TotalPower Exponential `json:"total_power"`
}

type Item struct {
	ItemID      int         `json:"item_id"`
	CountBought int         `json:"count_bought"`
	CountBuilt  int         `json:"count_built"`
	NextPrice   Exponential `json:"next_price"`
	Power       Exponential `json:"power"`
	Building    []Building  `json:"building"`
}

type OnSale struct {
	ItemID int   `json:"item_id"`
	Time   int64 `json:"time"`
}

type Building struct {
	Time       int64       `json:"time"`
	CountBuilt int         `json:"count_built"`
	Power      Exponential `json:"power"`
}

type GameStatus struct {
	Time     int64      `json:"time"`
	Adding   []Adding   `json:"adding"`
	Schedule []Schedule `json:"schedule"`
	Items    []Item     `json:"items"`
	OnSale   []OnSale   `json:"on_sale"`
}

type mItem struct {
	ItemID int   `db:"item_id"`
	Power1 int64 `db:"power1"`
	Power2 int64 `db:"power2"`
	Power3 int64 `db:"power3"`
	Power4 int64 `db:"power4"`
	Price1 int64 `db:"price1"`
	Price2 int64 `db:"price2"`
	Price3 int64 `db:"price3"`
	Price4 int64 `db:"price4"`
}

func str2bigx1000(s string) *big.Int {
	x := new(big.Int)
	x.SetString(s+"000", 10)
	return x
}

func (item *mItem) GetPower(count int) *big.Int {
	// power(x):=(cx+1)*d^(ax+b)
	a := item.Power1
	b := item.Power2
	c := item.Power3
	d := item.Power4
	x := int64(count)

	s := big.NewInt(c*x + 1)
	t := new(big.Int).Exp(big.NewInt(d), big.NewInt(a*x+b), nil)
	return new(big.Int).Mul(s, t)
}

func (item *mItem) GetPrice(count int) *big.Int {
	// price(x):=(cx+1)*d^(ax+b)
	a := item.Price1
	b := item.Price2
	c := item.Price3
	d := item.Price4
	x := int64(count)

	s := big.NewInt(c*x + 1)
	t := new(big.Int).Exp(big.NewInt(d), big.NewInt(a*x+b), nil)
	return new(big.Int).Mul(s, t)
}

func str2big(s string) *big.Int {
	x := new(big.Int)
	x.SetString(s, 10)
	return x
}

// 桁数が増えても大丈夫なBigintのDiv
func customBigIntDiv(a *big.Int, b *big.Int) int64 {
	alen := len(a.Bits())
	if alen < 4 {
		return big.NewInt(0).Div(a, b).Int64()
	}
	// WARN: ここらへんの big.NewInt(0)をグローバルに持てればより高速化出来るが並列化出来なくなるので諦め
	an := big.NewInt(0).SetBits(a.Bits()[alen-3:])
	bn := big.NewInt(0).SetBits(b.Bits()[alen-3:])
	return big.NewInt(0).Div(an, bn).Int64()
}

// int64をそのまま文字列化すると15桁以上になりうるので調整が必要
func int64ToExponential(significand, exponent int64) Exponential {
	var addketa int64
	var divten int64
	if significand < 1000000000000000 {
		addketa, divten = 0, 1
	} else if significand < 10000000000000000 {
		addketa, divten = 1, 10
	} else if significand < 100000000000000000 {
		addketa, divten = 2, 100
	} else if significand < 1000000000000000000 {
		addketa, divten = 3, 1000
	} else {
		addketa, divten = 4, 10000
	}
	return Exponential{significand / divten, exponent + addketa}
	}

func setupTenCache() []big.Int {
	log.Println("start setupTenCache")
	var tenCache = make([]big.Int, cacheSize) // メモリに応じて適宜調整のこと
	bigTen := big.NewInt(10)
	tenCache[0].Exp(bigTen, big.NewInt(int64(0)), nil)
	for i := 1; i < len(tenCache); i++ {
		tenCache[i].Mul(bigTen, &tenCache[i-1])
	}
	log.Println("end setupTenCache")
	return tenCache
}

// WARN: 10^n をキャッシュして使いまわす(初期化に数秒かかる/メモリを喰うので適宜調整のこと)
var tenCache = setupTenCache()
var ten = big.NewInt(10)
func big2expCustom(n *big.Int) Exponential {
	w := n.Bits()
	if len(w) <= 1 {
		return int64ToExponential(n.Int64(), 0)
	}
	w1 := float64(w[len(w)-1]) // 上のケタ
	w2 := float64(w[len(w)-2]) // 下のケタ
	bef := len(w) - 2
	log10ed := math.Log10(2) * 64 * float64(bef)
	log10ed += math.Log10(float64(1<<64)*w1 + w1 + w2)
	keta := int64(log10ed - 14.0)
	if keta < int64(len(tenCache)) {
		// WARN: ここらへんの big.NewInt(0)をグローバルに持てればより高速化出来るが並列化出来なくなるので諦め
		ketaInt := &tenCache[keta]
		significand := customBigIntDiv(n,ketaInt)
		return int64ToExponential(significand, keta)
	} else {
		ketaInt := big.NewInt(0).Exp(ten, big.NewInt(keta), nil)
		significand := customBigIntDiv(n, ketaInt)
		return int64ToExponential(significand, keta)
	}
}

func getCurrentTime() (int64, error) {
	var currentTime int64
	currentTime = time.Now().UnixNano() / int64(time.Millisecond)
	return currentTime, nil
}

// 部屋のロックを取りタイムスタンプを更新する
//
// トランザクション開始後この関数を呼ぶ前にクエリを投げると、
// そのトランザクション中の通常のSELECTクエリが返す結果がロック取得前の
// 状態になることに注意 (keyword: MVCC, repeatable read).
func updateRoomTime(tx *sqlx.Tx, roomName string, reqTime int64) (int64, bool) {
	// See page 13 and 17 in https://www.slideshare.net/ichirin2501/insert-51938787
	_, err := tx.Exec("INSERT INTO room_time(room_name, time) VALUES (?, 0) ON DUPLICATE KEY UPDATE time = time", roomName)
	if err != nil {
		log.Println(err)
		return 0, false
	}

	var roomTime int64
	err = tx.Get(&roomTime, "SELECT time FROM room_time WHERE room_name = ? FOR UPDATE", roomName)
	if err != nil {
		log.Println(err)
		return 0, false
	}

	var currentTime int64
	currentTime = time.Now().UnixNano() / int64(time.Millisecond)

	if roomTime > currentTime {
		log.Println("room time is future")
		return 0, false
	}
	if reqTime != 0 {
		if reqTime < currentTime {
			log.Println("reqTime is past")
			return 0, false
		}
	}

	_, err = tx.Exec("UPDATE room_time SET time = ? WHERE room_name = ?", currentTime, roomName)
	if err != nil {
		log.Println(err)
		return 0, false
	}

	return currentTime, true
}

func addIsu(roomName string, reqIsu *big.Int, reqTime int64) bool {
	tx, err := db.Beginx()
	if err != nil {
		log.Println(err)
		return false
	}

	_, ok := updateRoomTime(tx, roomName, reqTime)
	if !ok {
		tx.Rollback()
		return false
	}

	_, err = tx.Exec("INSERT INTO adding(room_name, time, isu) VALUES (?, ?, '0') ON DUPLICATE KEY UPDATE isu=isu", roomName, reqTime)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}

	var isuStr string
	err = tx.QueryRow("SELECT isu FROM adding WHERE room_name = ? AND time = ? FOR UPDATE", roomName, reqTime).Scan(&isuStr)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}
	isu := str2big(isuStr)

	isu.Add(isu, reqIsu)
	_, err = tx.Exec("UPDATE adding SET isu = ? WHERE room_name = ? AND time = ?", isu.String(), roomName, reqTime)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}

	if err := tx.Commit(); err != nil {
		log.Println(err)
		return false
	}
	return true
}

func buyItem(roomName string, itemID int, countBought int, reqTime int64) bool {
	tx, err := db.Beginx()
	if err != nil {
		log.Println(err)
		return false
	}

	_, ok := updateRoomTime(tx, roomName, reqTime)
	if !ok {
		tx.Rollback()
		return false
	}

	var countBuying int
	err = tx.Get(&countBuying, "SELECT COUNT(*) FROM buying WHERE room_name = ? AND item_id = ?", roomName, itemID)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}
	if countBuying != countBought {
		tx.Rollback()
		log.Println(roomName, itemID, countBought+1, " is already bought")
		return false
	}

	totalMilliIsu := new(big.Int)
	var addings []Adding
	err = tx.Select(&addings, "SELECT isu FROM adding WHERE room_name = ? AND time <= ?", roomName, reqTime)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}

	for _, a := range addings {
		totalMilliIsu.Add(totalMilliIsu, str2bigx1000(a.Isu))
	}

	var buyings []Buying
	err = tx.Select(&buyings, "SELECT item_id, ordinal, time FROM buying WHERE room_name = ?", roomName)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}
	for _, b := range buyings {
		var item mItem
		item = mItems[b.ItemID]
		cost := new(big.Int).Mul(item.GetPrice(b.Ordinal), big1000)
		totalMilliIsu.Sub(totalMilliIsu, cost)
		if b.Time <= reqTime {
			gain := new(big.Int).Mul(item.GetPower(b.Ordinal), big.NewInt(reqTime-b.Time))
			totalMilliIsu.Add(totalMilliIsu, gain)
		}
	}

	var item mItem
	item = mItems[itemID]
	need := new(big.Int).Mul(item.GetPrice(countBought+1), big1000)
	if totalMilliIsu.Cmp(need) < 0 {
		log.Println("not enough")
		tx.Rollback()
		return false
	}

	_, err = tx.Exec("INSERT INTO buying(room_name, item_id, ordinal, time) VALUES(?, ?, ?, ?)", roomName, itemID, countBought+1, reqTime)
	if err != nil {
		log.Println(err)
		tx.Rollback()
		return false
	}

	if err := tx.Commit(); err != nil {
		log.Println(err)
		return false
	}

	return true
}

func getStatusWithGroup(roomName string) (*GameStatus, error) {
	v, err, shared := group.Do(roomName, func() (interface{}, error) {
		return getStatus(roomName)
	})
	if err != nil {
		return nil, err
	}
	status, ok := v.(*GameStatus)
	if !ok {
		return nil, fmt.Errorf("Failed to assert v")
	}
	log.Println("getStatusWithGroup::room:", roomName)
	log.Println("getStatusWithGroup::shared:", shared)
	return status, nil
}


func cmpx1000(x *big.Int, y *big.Int) bool {
	// calc: x >= y * 1000
	xw, yw := x.Bits(), y.Bits()
	xlen, ylen := len(xw), len(yw)
	// 極端に桁が違うものはすぐに省ける
	if xlen > ylen+1 {
		return true // x:[x1 x2 x3] y:[y1]
	}
	if xlen < ylen {
		return false // x:[x1] y:[y1 y2]
	}

	// それとなく近いわけでは無いものを除外
	xw1 := xw[xlen-1] // X上のケタ (> 0)
	yw1 := yw[ylen-1] // Y上のケタ (> 0)
	if xlen == ylen {
		if xw1/1000 > yw1 {
			return true // 102456  102.456
		} else if xw1/1000 < yw1 {
			return false
		}
	}else{
		// xlen == ylen+1
		if xw1 >= 1000 {
			return true
		}
		// なんかダメ
		// if yw1 < math.MaxUint64/1000  { return false}
	}
	// 近すぎるので普通に計算
	return 0 <= x.Cmp(new(big.Int).Mul(y, big1000))
}


func getStatus(roomName string) (*GameStatus, error) {
	tx, err := db.Beginx()
	if err != nil {
		return nil, err
	}

	currentTime, ok := updateRoomTime(tx, roomName, 0)
	if !ok {
		tx.Rollback()
		return nil, fmt.Errorf("updateRoomTime failure")
	}

	addings := []Adding{}
	err = tx.Select(&addings, "SELECT time, isu FROM adding WHERE room_name = ?", roomName)
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	buyings := []Buying{}
	err = tx.Select(&buyings, "SELECT item_id, ordinal, time FROM buying WHERE room_name = ?", roomName)
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, err
	}

	status, err := calcStatus(currentTime, mItems, addings, buyings)
	if err != nil {
		return nil, err
	}

	// calcStatusに時間がかかる可能性があるので タイムスタンプを取得し直す
	latestTime, err := getCurrentTime()
	if err != nil {
		return nil, err
	}

	status.Time = latestTime
	return status, err
}

func calcStatus(currentTime int64, mItems map[int]mItem, addings []Adding, buyings []Buying) (*GameStatus, error) {
	var (
		// 1ミリ秒に生産できる椅子の単位をミリ椅子とする
		totalMilliIsu = big.NewInt(0)
		totalPower    = big.NewInt(0)

		itemPower    = map[int]*big.Int{}    // ItemID => Power
		itemPrice    = map[int]*big.Int{}    // ItemID => Price
		itemOnSale   = map[int]int64{}       // ItemID => OnSale
		itemBuilt    = map[int]int{}         // ItemID => BuiltCount
		itemBought   = map[int]int{}         // ItemID => CountBought
		itemBuilding = map[int][]Building{}  // ItemID => Buildings
		itemPower0   = map[int]Exponential{} // ItemID => currentTime における Power
		itemBuilt0   = map[int]int{}         // ItemID => currentTime における BuiltCount

		addingAt = map[int64]Adding{}   // Time => currentTime より先の Adding
		buyingAt = map[int64][]Buying{} // Time => currentTime より先の Buying
	)

	for itemID := range mItems {
		itemPower[itemID] = big.NewInt(0)
		itemBuilding[itemID] = []Building{}
	}

	for _, a := range addings {
		// adding は adding.time に isu を増加させる
		if a.Time <= currentTime {
			totalMilliIsu.Add(totalMilliIsu, str2bigx1000(a.Isu))
		} else {
			addingAt[a.Time] = a
		}
	}

	for _, b := range buyings {
		// buying は 即座に isu を消費し buying.time からアイテムの効果を発揮する
		itemBought[b.ItemID]++
		m := mItems[b.ItemID]
		totalMilliIsu.Sub(totalMilliIsu, new(big.Int).Mul(m.GetPrice(b.Ordinal), big1000))

		if b.Time <= currentTime {
			itemBuilt[b.ItemID]++
			power := m.GetPower(itemBought[b.ItemID])
			totalMilliIsu.Add(totalMilliIsu, new(big.Int).Mul(power, big.NewInt(currentTime-b.Time)))
			totalPower.Add(totalPower, power)
			itemPower[b.ItemID].Add(itemPower[b.ItemID], power)
		} else {
			buyingAt[b.Time] = append(buyingAt[b.Time], b)
		}
	}

	for _, m := range mItems {
		itemPower0[m.ItemID] = big2expCustom(itemPower[m.ItemID])
		itemBuilt0[m.ItemID] = itemBuilt[m.ItemID]
		price := m.GetPrice(itemBought[m.ItemID] + 1)
		itemPrice[m.ItemID] = price
		//if 0 <= totalMilliIsu.Cmp(new(big.Int).Mul(price, big1000)) {
		if cmpx1000(totalMilliIsu,price){
			itemOnSale[m.ItemID] = 0 // 0 は 時刻 currentTime で購入可能であることを表す
		}
	}

	schedule := []Schedule{
		Schedule{
			Time:       currentTime,
			MilliIsu:   big2expCustom(totalMilliIsu),
			TotalPower: big2expCustom(totalPower),
		},
	}

	// currentTime から 1000 ミリ秒先までシミュレーションする
	for t := currentTime + 1; t <= currentTime+1000; t++ {
		totalMilliIsu.Add(totalMilliIsu, totalPower)
		updated := false

		// 時刻 t で発生する adding を計算する
		if a, ok := addingAt[t]; ok {
			updated = true
			totalMilliIsu.Add(totalMilliIsu, str2bigx1000(a.Isu))
		}

		// 時刻 t で発生する buying を計算する
		if _, ok := buyingAt[t]; ok {
			updated = true
			updatedID := map[int]bool{}
			for _, b := range buyingAt[t] {
				m := mItems[b.ItemID]
				updatedID[b.ItemID] = true
				itemBuilt[b.ItemID]++
				power := m.GetPower(b.Ordinal)
				itemPower[b.ItemID].Add(itemPower[b.ItemID], power)
				totalPower.Add(totalPower, power)
			}
			for id := range updatedID {
				itemBuilding[id] = append(itemBuilding[id], Building{
					Time:       t,
					CountBuilt: itemBuilt[id],
					Power:      big2expCustom(itemPower[id]),
				})
			}
		}

		if updated {
			schedule = append(schedule, Schedule{
				Time:       t,
				MilliIsu:   big2expCustom(totalMilliIsu),
				TotalPower: big2expCustom(totalPower),
			})
		}

		// 時刻 t で購入可能になったアイテムを記録する
		for itemID := range mItems {
			if _, ok := itemOnSale[itemID]; ok {
				continue
			}
			if cmpx1000(totalMilliIsu, itemPrice[itemID]) {
				itemOnSale[itemID] = t
			}
		}
	}

	gsAdding := []Adding{}
	for _, a := range addingAt {
		gsAdding = append(gsAdding, a)
	}

	gsItems := []Item{}
	for itemID, _ := range mItems {
		gsItems = append(gsItems, Item{
			ItemID:      itemID,
			CountBought: itemBought[itemID],
			CountBuilt:  itemBuilt0[itemID],
			NextPrice:   big2expCustom(itemPrice[itemID]),
			Power:       itemPower0[itemID],
			Building:    itemBuilding[itemID],
		})
	}

	gsOnSale := []OnSale{}
	for itemID, t := range itemOnSale {
		gsOnSale = append(gsOnSale, OnSale{
			ItemID: itemID,
			Time:   t,
		})
	}

	return &GameStatus{
		Adding:   gsAdding,
		Schedule: schedule,
		Items:    gsItems,
		OnSale:   gsOnSale,
	}, nil
}

func roomHandler(roomName string, room Room) {
	closeCh := make(chan struct{})
	go func() {
		room.wg.Wait()
		close(closeCh)
	}()

	ticker := time.NewTicker(duration)

	for {
		select {
		case <-ticker.C:
			group.Forget(roomName)
			room.c.Broadcast()
		case <-closeCh:
			rooms.Delete(roomName)
			return
		}
	}
}

func serveGameConn(ws *websocket.Conn, roomName string) {
	log.Println(ws.RemoteAddr(), "serveGameConn", roomName)
	defer ws.Close()

	v, loaded := rooms.LoadOrStore(roomName, Room{new(sync.WaitGroup), sync.NewCond(new(sync.Mutex))})
	room := v.(Room)
	room.wg.Add(1)
	defer room.wg.Done()
	if !loaded {
		go roomHandler(roomName, room)
	}

	status, err := getStatusWithGroup(roomName)
	if err != nil {
		log.Println(err)
		return
	}

	err = ws.WriteJSON(status)
	if err != nil {
		log.Println(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chReq := make(chan GameRequest)

	go func() {
		defer cancel()
		for {
			req := GameRequest{}
			err := ws.ReadJSON(&req)
			if err != nil {
				log.Println(err)
				return
			}

			select {
			case chReq <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case req := <-chReq:
			log.Println(req)

			success := false
			switch req.Action {
			case "addIsu":
				success = addIsu(roomName, str2big(req.Isu), req.Time)
			case "buyItem":
				success = buyItem(roomName, req.ItemID, req.CountBought, req.Time)
			default:
				log.Println("Invalid Action")
				return
			}

			if success {
				// GameResponse を返却する前に 反映済みの GameStatus を返す
				room.c.L.Lock()
				room.c.Wait()
				room.c.L.Unlock()
				status, err := getStatusWithGroup(roomName)
				if err != nil {
					log.Println(err)
					return
				}

				err = ws.WriteJSON(status)
				if err != nil {
					log.Println(err)
					return
				}
			}

			err := ws.WriteJSON(GameResponse{
				RequestID: req.RequestID,
				IsSuccess: success,
			})
			if err != nil {
				log.Println(err)
				return
			}
		case <-ticker.C:
			status, err := getStatusWithGroup(roomName)
			if err != nil {
				log.Println(err)
				return
			}

			err = ws.WriteJSON(status)
			if err != nil {
				log.Println(err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
