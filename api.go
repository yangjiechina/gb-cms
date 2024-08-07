package main

import (
	"context"
	"fmt"
	"gb-cms/sdp"
	"github.com/ghettovoice/gosip"
	"github.com/ghettovoice/gosip/sip"
	"github.com/gorilla/mux"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ApiServer struct {
	router *mux.Router
}

var apiServer *ApiServer

func init() {
	apiServer = &ApiServer{
		router: mux.NewRouter(),
	}
}

func withCheckParams(f func(streamId, protocol string, w http.ResponseWriter, req *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		if "" != req.URL.RawQuery {
			Sugar.Infof("on request %s?%s", req.URL.Path, req.URL.RawQuery)
		}

		v := struct {
			Stream     string `json:"stream"`      //Stream id
			Protocol   string `json:"protocol"`    //推拉流协议
			RemoteAddr string `json:"remote_addr"` //peer地址
		}{}

		err := HttpDecodeJSONBody(w, req, &v)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		f(v.Stream, v.Protocol, w, req)
	}
}

func startApiServer(addr string) {
	apiServer.router.HandleFunc("/api/v1/hook/on_play", withCheckParams(apiServer.OnPlay))
	apiServer.router.HandleFunc("/api/v1/hook/on_play_done", withCheckParams(apiServer.OnPlayDone))
	apiServer.router.HandleFunc("/api/v1/hook/on_publish", withCheckParams(apiServer.OnPublish))
	apiServer.router.HandleFunc("/api/v1/hook/on_publish_done", withCheckParams(apiServer.OnPublishDone))
	apiServer.router.HandleFunc("/api/v1/hook/on_idle_timeout", withCheckParams(apiServer.OnIdleTimeout))
	apiServer.router.HandleFunc("/api/v1/hook/on_receive_timeout", withCheckParams(apiServer.OnReceiveTimeout))

	apiServer.router.HandleFunc("/api/v1/device/list", apiServer.OnDeviceList)         //查询在线设备
	apiServer.router.HandleFunc("/api/v1/record/list", apiServer.OnRecordList)         //查询录像列表
	apiServer.router.HandleFunc("/api/v1/position/sub", apiServer.OnSubscribePosition) //订阅移动位置
	apiServer.router.HandleFunc("/api/v1/playback/seek", apiServer.OnSeekPlayback)     //回放seek

	apiServer.router.HandleFunc("/api/v1/ptz/control", apiServer.OnPTZControl) //云台控制
	apiServer.router.HandleFunc("/api/v1/broadcast", apiServer.OnBroadcast)    //语音广播
	apiServer.router.HandleFunc("/api/v1/talk", apiServer.OnTalk)              //语音对讲
	http.Handle("/", apiServer.router)

	srv := &http.Server{
		Handler: apiServer.router,
		Addr:    addr,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 30 * time.Second,
		ReadTimeout:  30 * time.Second,
	}

	err := srv.ListenAndServe()

	if err != nil {
		panic(err)
	}
}

func (api *ApiServer) OnPlay(streamId, protocol string, w http.ResponseWriter, r *http.Request) {
	Sugar.Infof("play. protocol:%s stream id:%s", protocol, streamId)

	//[注意]: windows上使用cmd/power shell推拉流如果要携带多个参数, 请用双引号将与号引起来("&")
	//session_id是为了同一个录像文件, 允许同时点播多个.当然如果实时流支持多路预览, 也是可以的.
	//ffplay -i rtmp://127.0.0.1/34020000001320000001/34020000001310000001
	//ffplay -i http://127.0.0.1:8080/34020000001320000001/34020000001310000001.flv?setup=passive
	//ffplay -i http://127.0.0.1:8080/34020000001320000001/34020000001310000001.m3u8?setup=passive
	//ffplay -i rtsp://test:123456@127.0.0.1/34020000001320000001/34020000001310000001?setup=passive

	//回放示例
	//ffplay -i rtmp://127.0.0.1/34020000001320000001/34020000001310000001.session_id_0?setup=passive"&"stream_type=playback"&"start_time=2024-06-18T15:20:56"&"end_time=2024-06-18T15:25:56
	//ffplay -i rtmp://127.0.0.1/34020000001320000001/34020000001310000001.session_id_0?setup=passive&stream_type=playback&start_time=2024-06-18T15:20:56&end_time=2024-06-18T15:25:56

	stream := StreamManager.Find(streamId)
	if stream != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	split := strings.Split(streamId, "/")
	if len(split) != 2 {
		w.WriteHeader(http.StatusOK)
		return
	}

	//跳过非国标拉流
	if len(split[0]) != 20 || len(split[1]) < 20 {
		w.WriteHeader(http.StatusOK)
		return
	}

	deviceId := split[0]  //deviceId
	channelId := split[1] //channelId
	device := DeviceManager.Find(deviceId)

	if len(channelId) > 20 {
		channelId = channelId[:20]
	}

	if device == nil {
		Sugar.Warnf("设备离线 id:%s", deviceId)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	stream = &Stream{Id: streamId, Protocol: "28181", ByeRequest: nil}
	if err := StreamManager.Add(stream); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	var inviteOk bool
	defer func() {
		if !inviteOk {
			api.CloseStream(streamId)
			go CloseGBSource(streamId)
		}
	}()

	query := r.URL.Query()
	setup := strings.ToLower(query.Get("setup"))
	streamType := strings.ToLower(query.Get("stream_type"))
	startTimeStr := strings.ToLower(query.Get("start_time"))
	endTimeStr := strings.ToLower(query.Get("end_time"))
	speedStr := strings.ToLower(query.Get("speed"))

	var startTimeSeconds string
	var endTimeSeconds string
	var err error
	var ssrc string
	if "playback" == streamType || "download" == streamType {
		startTime, err := time.ParseInLocation("2006-01-02t15:04:05", startTimeStr, time.Local)
		if err != nil {
			Sugar.Errorf("解析开始时间失败 err:%s start_time:%s", err.Error(), startTimeStr)
			return
		}
		endTime, err := time.ParseInLocation("2006-01-02t15:04:05", endTimeStr, time.Local)
		if err != nil {
			Sugar.Errorf("解析开始时间失败 err:%s start_time:%s", err.Error(), startTimeStr)
			return
		}

		startTimeSeconds = strconv.FormatInt(startTime.Unix(), 10)
		endTimeSeconds = strconv.FormatInt(endTime.Unix(), 10)
		ssrc = GetVodSSRC()
	} else {
		ssrc = GetLiveSSRC()
	}

	ssrcValue, _ := strconv.Atoi(ssrc)
	ip, port, err := CreateGBSource(streamId, setup, uint32(ssrcValue))
	if err != nil {
		Sugar.Errorf("创建GBSource失败 err:%s", err.Error())
		return
	}

	var inviteRequest sip.Request
	if "playback" == streamType {
		inviteRequest, err = device.BuildPlaybackRequest(channelId, ip, port, startTimeSeconds, endTimeSeconds, setup, ssrc)
	} else if "download" == streamType {
		speed, _ := strconv.Atoi(speedStr)
		speed = int(math.Min(4, float64(speed)))
		inviteRequest, err = device.BuildDownloadRequest(channelId, ip, port, startTimeSeconds, endTimeSeconds, setup, speed, ssrc)
	} else {
		inviteRequest, err = device.BuildLiveRequest(channelId, ip, port, setup, ssrc)
	}

	if err != nil {
		return
	}

	var bye sip.Request
	var answer string
	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	SipUA.SendRequestWithContext(reqCtx, inviteRequest, gosip.WithResponseHandler(func(res sip.Response, request sip.Request) {
		if res.StatusCode() < 200 {

		} else if res.StatusCode() == 200 {
			answer = res.Body()
			ackRequest := sip.NewAckRequest("", inviteRequest, res, "", nil)
			ackRequest.AppendHeader(globalContactAddress.AsContactHeader())
			//手动替换ack请求目标地址, answer的contact可能不对.
			recipient := ackRequest.Recipient()
			recipient.SetHost(Config.PublicIP)
			recipient.SetPort(&Config.SipPort)

			Sugar.Infof("send ack %s", ackRequest.String())

			err := SipUA.Send(ackRequest)
			if err != nil {
				cancel()
				Sugar.Errorf("send ack error %s %s", err.Error(), ackRequest.String())
			} else {
				inviteOk = true
				bye = ackRequest.Clone().(sip.Request)
				bye.SetMethod(sip.BYE)
				bye.RemoveHeader("Via")
				if seq, ok := bye.CSeq(); ok {
					seq.SeqNo++
					seq.MethodName = sip.BYE
				}
			}
		} else if res.StatusCode() > 299 {
			cancel()
		}
	}))

	if !inviteOk {
		return
	}

	if "active" == setup {
		parse, err := sdp.Parse(answer)
		if err != nil {
			inviteOk = false
			Sugar.Errorf("解析应答sdp失败 err:%s sdp:%s", err.Error(), answer)
			return
		}
		if parse.Video == nil || parse.Video.Port == 0 {
			inviteOk = false
			Sugar.Errorf("应答没有视频连接地址 sdp:%s", answer)
			return
		}

		addr := fmt.Sprintf("%s:%d", parse.Addr, parse.Video.Port)
		if err = ConnectGBSource(streamId, addr); err != nil {
			inviteOk = false
			Sugar.Errorf("设置GB28181连接地址失败 err:%s addr:%s", err.Error(), addr)
		}
	}

	if stream.waitPublishStream() {
		stream.ByeRequest = bye
		w.WriteHeader(http.StatusOK)
	} else {
		SipUA.SendRequest(bye)
	}
}

func (api *ApiServer) CloseStream(streamId string) {
	stream, _ := StreamManager.Remove(streamId)
	if stream != nil && stream.ByeRequest != nil {
		SipUA.SendRequest(stream.ByeRequest)
		return
	}
}

func (api *ApiServer) OnPlayDone(streamId, protocol string, w http.ResponseWriter, r *http.Request) {
	Sugar.Infof("play done. protocol:%s stream id:%s", protocol, streamId)
	w.WriteHeader(http.StatusOK)
}

func (api *ApiServer) OnPublish(streamId, protocol string, w http.ResponseWriter, r *http.Request) {
	Sugar.Infof("publish. protocol:%s stream id:%s", protocol, streamId)

	w.WriteHeader(http.StatusOK)
	stream := StreamManager.Find(streamId)
	if stream != nil {
		stream.publishEvent <- 0
	}
}

func (api *ApiServer) OnPublishDone(streamId, protocol string, w http.ResponseWriter, r *http.Request) {
	Sugar.Infof("publish done. protocol:%s stream id:%s", protocol, streamId)

	w.WriteHeader(http.StatusOK)
	api.CloseStream(streamId)
}

func (api *ApiServer) OnIdleTimeout(streamId string, protocol string, w http.ResponseWriter, req *http.Request) {
	Sugar.Infof("publish timeout. protocol:%s stream id:%s", protocol, streamId)

	if protocol != "rtmp" {
		w.WriteHeader(http.StatusForbidden)
		api.CloseStream(streamId)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func (api *ApiServer) OnReceiveTimeout(streamId string, protocol string, w http.ResponseWriter, req *http.Request) {
	Sugar.Infof("receive timeout. protocol:%s stream id:%s", protocol, streamId)

	if protocol != "rtmp" {
		w.WriteHeader(http.StatusForbidden)
		api.CloseStream(streamId)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func (api *ApiServer) OnDeviceList(w http.ResponseWriter, r *http.Request) {
	devices := DeviceManager.AllDevices()
	httpResponseOK(w, devices)
}

func (api *ApiServer) OnRecordList(w http.ResponseWriter, r *http.Request) {
	v := struct {
		DeviceId  string `json:"device_id"`
		ChannelId string `json:"channel_id"`
		Timeout   int    `json:"timeout"`
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
		Type_     string `json:"type"`
	}{}

	err := HttpDecodeJSONBody(w, r, &v)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	device := DeviceManager.Find(v.DeviceId)
	if device == nil {
		httpResponseOK(w, "设备离线")
		return
	}

	sn := GetSN()
	err = device.DoRecordList(v.ChannelId, v.StartTime, v.EndTime, sn, v.Type_)
	if err != nil {
		httpResponseOK(w, fmt.Sprintf("发送查询录像记录失败 err:%s", err.Error()))
		return
	}

	var recordList []RecordInfo
	timeout := int(math.Max(math.Min(5, float64(v.Timeout)), 60))
	//设置查询超时时长
	withTimeout, cancelFunc := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)

	SNManager.AddEvent(sn, func(data interface{}) {
		response := data.(*QueryRecordInfoResponse)

		if len(response.DeviceList.Devices) > 0 {
			recordList = append(recordList, response.DeviceList.Devices...)
		}

		//查询完成
		if len(recordList) >= response.SumNum {
			cancelFunc()
		}
	})

	select {
	case _ = <-withTimeout.Done():
		break
	}

	httpResponseOK(w, recordList)
}

func (api *ApiServer) OnSubscribePosition(w http.ResponseWriter, r *http.Request) {
	v := struct {
		DeviceID  string `json:"device_id"`
		ChannelID string `json:"channel_id"`
	}{}

	if err := HttpDecodeJSONBody(w, r, &v); err != nil {
		httpResponse2(w, err)
		return
	}

	device := DeviceManager.Find(v.DeviceID)
	if device == nil {
		return
	}

	if err := device.DoSubscribePosition(v.ChannelID); err != nil {

	}

	w.WriteHeader(http.StatusOK)
}

func (api *ApiServer) OnSeekPlayback(w http.ResponseWriter, r *http.Request) {
	devices := DeviceManager.AllDevices()
	httpResponse2(w, devices)
}

func (api *ApiServer) OnPTZControl(w http.ResponseWriter, r *http.Request) {
	devices := DeviceManager.AllDevices()
	httpResponse2(w, devices)
}

func (api *ApiServer) OnBroadcast(w http.ResponseWriter, r *http.Request) {
	//v := struct {
	//	DeviceID  string `json:"device_id"`
	//	ChannelID string `json:"channel_id"`
	//	RoomID    string `json:"room_id"` //如果要实现群呼功能, 除第一次广播外, 后续请求都携带该参数
	//}{}

}

func (api *ApiServer) OnTalk(w http.ResponseWriter, r *http.Request) {
	devices := DeviceManager.AllDevices()
	httpResponse2(w, devices)
}
