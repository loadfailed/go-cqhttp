package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mrs4s/go-cqhttp/coolq"
	"github.com/Mrs4s/go-cqhttp/global"
	"github.com/gin-gonic/gin"
	"github.com/guonaihong/gout"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type httpServer struct {
	engine *gin.Engine
	bot    *coolq.CQBot
	Http   *http.Server
}

type httpClient struct {
	bot     *coolq.CQBot
	secret  string
	addr    string
	timeout int32
}

var HttpServer = &httpServer{}

func (s *httpServer) Run(addr, authToken string, bot *coolq.CQBot) {
	gin.SetMode(gin.ReleaseMode)
	s.engine = gin.New()
	s.bot = bot
	s.engine.Use(func(c *gin.Context) {
		if c.Request.Method != "GET" && c.Request.Method != "POST" {
			log.Warnf("已拒绝客户端 %v 的请求: 方法错误", c.Request.RemoteAddr)
			c.Status(404)
			return
		}
		if c.Request.Method == "POST" && strings.Contains(c.Request.Header.Get("Content-Type"), "application/json") {
			d, err := c.GetRawData()
			if err != nil {
				log.Warnf("获取请求 %v 的Body时出现错误: %v", c.Request.RequestURI, err)
				c.Status(400)
				return
			}
			if !gjson.ValidBytes(d) {
				log.Warnf("已拒绝客户端 %v 的请求: 非法Json", c.Request.RemoteAddr)
				c.Status(400)
				return
			}
			c.Set("json_body", gjson.ParseBytes(d))
		}
		c.Next()
	})

	if authToken != "" {
		s.engine.Use(func(c *gin.Context) {
			if auth := c.Request.Header.Get("Authorization"); auth != "" {
				if strings.SplitN(auth, " ", 2)[1] != authToken {
					c.AbortWithStatus(401)
					return
				}
			} else if c.Query("access_token") != authToken {
				c.AbortWithStatus(401)
				return
			} else {
				c.Next()
			}
		})
	}

	s.engine.Any("/:action", s.HandleActions)

	go func() {
		log.Infof("CQ HTTP 服务器已启动: %v", addr)
		s.Http = &http.Server{
			Addr:    addr,
			Handler: s.engine,
		}
		if err := s.Http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(err)
			log.Infof("请检查端口是否被占用.")
			time.Sleep(time.Second * 5)
			os.Exit(1)
		}
		//err := s.engine.Run(addr)
		//if err != nil {
		//	log.Error(err)
		//	log.Infof("请检查端口是否被占用.")
		//	time.Sleep(time.Second * 5)
		//	os.Exit(1)
		//}
	}()
}

func NewHttpClient() *httpClient {
	return &httpClient{}
}

func (c *httpClient) Run(addr, secret string, timeout int32, bot *coolq.CQBot) {
	c.bot = bot
	c.secret = secret
	c.addr = addr
	c.timeout = timeout
	if c.timeout < 5 {
		c.timeout = 5
	}
	bot.OnEventPush(c.onBotPushEvent)
	log.Infof("HTTP POST上报器已启动: %v", addr)
}

func (c *httpClient) onBotPushEvent(m coolq.MSG) {
	var res string
	err := gout.POST(c.addr).SetJSON(m).BindBody(&res).SetHeader(func() gout.H {
		h := gout.H{
			"X-Self-ID":  c.bot.Client.Uin,
			"User-Agent": "CQHttp/4.15.0",
		}
		if c.secret != "" {
			mac := hmac.New(sha1.New, []byte(c.secret))
			mac.Write([]byte(m.ToJson()))
			h["X-Signature"] = "sha1=" + hex.EncodeToString(mac.Sum(nil))
		}
		return h
	}()).SetTimeout(time.Second * time.Duration(c.timeout)).F().Retry().Attempt(5).
		WaitTime(time.Millisecond * 500).MaxWaitTime(time.Second * 5).
		Do()
	if err != nil {
		log.Warnf("上报Event数据 %v 到 %v 失败: %v", m.ToJson(), c.addr, err)
		return
	}
	if gjson.Valid(res) {
		c.bot.CQHandleQuickOperation(gjson.Parse(m.ToJson()), gjson.Parse(res))
	}
}

func (s *httpServer) HandleActions(c *gin.Context) {
	global.RateLimit(context.Background())
	action := strings.ReplaceAll(c.Param("action"), "_async", "")
	log.Debugf("HTTPServer接收到API调用: %v", action)
	if f, ok := httpApi[action]; ok {
		f(s, c)
	} else {
		c.JSON(200, coolq.Failed(404))
	}
}

func (s *httpServer) GetLoginInfo(c *gin.Context) {
	c.JSON(200, s.bot.CQGetLoginInfo())
}

func (s *httpServer) GetFriendList(c *gin.Context) {
	c.JSON(200, s.bot.CQGetFriendList())
}

func (s *httpServer) GetGroupList(c *gin.Context) {
	nc := getParamOrDefault(c, "no_cache", "false")
	c.JSON(200, s.bot.CQGetGroupList(nc == "true"))
}

func (s *httpServer) GetGroupInfo(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	c.JSON(200, s.bot.CQGetGroupInfo(gid))
}

func (s *httpServer) GetGroupMemberList(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	nc := getParamOrDefault(c, "no_cache", "false")
	c.JSON(200, s.bot.CQGetGroupMemberList(gid, nc == "true"))
}

func (s *httpServer) GetGroupMemberInfo(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	c.JSON(200, s.bot.CQGetGroupMemberInfo(gid, uid))
}

func (s *httpServer) SendMessage(c *gin.Context) {
	if getParam(c, "message_type") == "private" {
		s.SendPrivateMessage(c)
		return
	}
	if getParam(c, "message_type") == "group" {
		s.SendGroupMessage(c)
		return
	}
	if getParam(c, "group_id") != "" {
		s.SendGroupMessage(c)
		return
	}
	if getParam(c, "user_id") != "" {
		s.SendPrivateMessage(c)
	}
}

func (s *httpServer) SendPrivateMessage(c *gin.Context) {
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	msg, t := getParamWithType(c, "message")
	autoEscape := global.EnsureBool(getParam(c, "auto_escape"), false)
	if t == gjson.JSON {
		c.JSON(200, s.bot.CQSendPrivateMessage(uid, gjson.Parse(msg), autoEscape))
		return
	}
	c.JSON(200, s.bot.CQSendPrivateMessage(uid, msg, autoEscape))
}

func (s *httpServer) SendGroupMessage(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	msg, t := getParamWithType(c, "message")
	autoEscape := global.EnsureBool(getParam(c, "auto_escape"), false)
	if t == gjson.JSON {
		c.JSON(200, s.bot.CQSendGroupMessage(gid, gjson.Parse(msg), autoEscape))
		return
	}
	c.JSON(200, s.bot.CQSendGroupMessage(gid, msg, autoEscape))
}

func (s *httpServer) SendGroupForwardMessage(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	msg := getParam(c, "messages")
	c.JSON(200, s.bot.CQSendGroupForwardMessage(gid, gjson.Parse(msg)))
}

func (s *httpServer) GetImage(c *gin.Context) {
	file := getParam(c, "file")
	c.JSON(200, s.bot.CQGetImage(file))
}

func (s *httpServer) GetGroupMessage(c *gin.Context) {
	mid, _ := strconv.ParseInt(getParam(c, "message_id"), 10, 32)
	c.JSON(200, s.bot.CQGetGroupMessage(int32(mid)))
}

func (s *httpServer) GetGroupHonorInfo(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	c.JSON(200, s.bot.CQGetGroupHonorInfo(gid, getParam(c, "type")))
}

func (s *httpServer) ProcessFriendRequest(c *gin.Context) {
	flag := getParam(c, "flag")
	approve := getParamOrDefault(c, "approve", "true")
	c.JSON(200, s.bot.CQProcessFriendRequest(flag, approve == "true"))
}

func (s *httpServer) ProcessGroupRequest(c *gin.Context) {
	flag := getParam(c, "flag")
	subType := getParam(c, "sub_type")
	if subType == "" {
		subType = getParam(c, "type")
	}
	approve := getParamOrDefault(c, "approve", "true")
	c.JSON(200, s.bot.CQProcessGroupRequest(flag, subType, getParam(c, "reason"), approve == "true"))
}

func (s *httpServer) SetGroupCard(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupCard(gid, uid, getParam(c, "card")))
}

func (s *httpServer) SetSpecialTitle(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupSpecialTitle(gid, uid, getParam(c, "special_title")))
}

func (s *httpServer) SetGroupKick(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	msg := getParam(c, "message")
	c.JSON(200, s.bot.CQSetGroupKick(gid, uid, msg))
}

func (s *httpServer) SetGroupBan(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	i, _ := strconv.ParseInt(getParamOrDefault(c, "duration", "1800"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupBan(gid, uid, uint32(i)))
}

func (s *httpServer) SetWholeBan(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupWholeBan(gid, getParamOrDefault(c, "enable", "true") == "true"))
}

func (s *httpServer) SetGroupName(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupName(gid, getParam(c, "group_name")))
}

func (s *httpServer) SetGroupAdmin(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupAdmin(gid, uid, getParamOrDefault(c, "enable", "true") == "true"))
}

func (s *httpServer) SendGroupNotice(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupMemo(gid, getParam(c, "content")))
}

func (s *httpServer) SetGroupLeave(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	c.JSON(200, s.bot.CQSetGroupLeave(gid))
}

func (s *httpServer) GetForwardMessage(c *gin.Context) {
	resId := getParam(c, "message_id")
	c.JSON(200, s.bot.CQGetForwardMessage(resId))
}

func (s *httpServer) DeleteMessage(c *gin.Context) {
	mid, _ := strconv.ParseInt(getParam(c, "message_id"), 10, 32)
	c.JSON(200, s.bot.CQDeleteMessage(int32(mid)))
}

func (s *httpServer) CanSendImage(c *gin.Context) {
	c.JSON(200, s.bot.CQCanSendImage())
}

func (s *httpServer) CanSendRecord(c *gin.Context) {
	c.JSON(200, s.bot.CQCanSendRecord())
}

func (s *httpServer) GetStatus(c *gin.Context) {
	c.JSON(200, s.bot.CQGetStatus())
}

func (s *httpServer) GetVersionInfo(c *gin.Context) {
	c.JSON(200, s.bot.CQGetVersionInfo())
}

func (s *httpServer) ReloadEventFilter(c *gin.Context) {
	c.JSON(200, s.bot.CQReloadEventFilter())
}

func (s *httpServer) GetVipInfo(c *gin.Context) {
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	c.JSON(200, s.bot.CQGetVipInfo(uid))
}

func (s *httpServer) GetStrangerInfo(c *gin.Context) {
	uid, _ := strconv.ParseInt(getParam(c, "user_id"), 10, 64)
	c.JSON(200, s.bot.CQGetStrangerInfo(uid))
}

func (s *httpServer) HandleQuickOperation(c *gin.Context) {
	if c.Request.Method != "POST" {
		c.AbortWithStatus(404)
		return
	}
	if i, ok := c.Get("json_body"); ok {
		body := i.(gjson.Result)
		c.JSON(200, s.bot.CQHandleQuickOperation(body.Get("context"), body.Get("operation")))
	}
}

func (s *httpServer) OcrImage(c *gin.Context) {
	img := getParam(c, "image")
	c.JSON(200, s.bot.CQOcrImage(img))
}

func (s *httpServer) GetWordSlices(c *gin.Context) {
	content := getParam(c, "content")
	c.JSON(200, s.bot.CQGetWordSlices(content))
}

func (s *httpServer) SetGroupPortrait(c *gin.Context) {
	gid, _ := strconv.ParseInt(getParam(c, "group_id"), 10, 64)
	file := getParam(c, "file")
	cache := getParam(c, "cache")
	c.JSON(200, s.bot.CQSetGroupPortrait(gid, file, cache))
}

func getParamOrDefault(c *gin.Context, k, def string) string {
	r := getParam(c, k)
	if r != "" {
		return r
	}
	return def
}

func getParam(c *gin.Context, k string) string {
	p, _ := getParamWithType(c, k)
	return p
}

func getParamWithType(c *gin.Context, k string) (string, gjson.Type) {
	if q := c.Query(k); q != "" {
		return q, gjson.Null
	}
	if c.Request.Method == "POST" {
		if h := c.Request.Header.Get("Content-Type"); h != "" {
			if strings.Contains(h, "application/x-www-form-urlencoded") {
				if p, ok := c.GetPostForm(k); ok {
					return p, gjson.Null
				}
			}
			if strings.Contains(h, "application/json") {
				if obj, ok := c.Get("json_body"); ok {
					res := obj.(gjson.Result).Get(k)
					if res.Exists() {
						switch res.Type {
						case gjson.JSON:
							return res.Raw, gjson.JSON
						case gjson.String:
							return res.Str, gjson.String
						case gjson.Number:
							return strconv.FormatInt(res.Int(), 10), gjson.Number // 似乎没有需要接受 float 类型的api
						case gjson.True:
							return "true", gjson.True
						case gjson.False:
							return "false", gjson.False
						}
					}
				}
			}
		}
	}
	return "", gjson.Null
}

var httpApi = map[string]func(s *httpServer, c *gin.Context){
	"get_login_info": func(s *httpServer, c *gin.Context) {
		s.GetLoginInfo(c)
	},
	"get_friend_list": func(s *httpServer, c *gin.Context) {
		s.GetFriendList(c)
	},
	"get_group_list": func(s *httpServer, c *gin.Context) {
		s.GetGroupList(c)
	},
	"get_group_info": func(s *httpServer, c *gin.Context) {
		s.GetGroupInfo(c)
	},
	"get_group_member_list": func(s *httpServer, c *gin.Context) {
		s.GetGroupMemberList(c)
	},
	"get_group_member_info": func(s *httpServer, c *gin.Context) {
		s.GetGroupMemberInfo(c)
	},
	"send_msg": func(s *httpServer, c *gin.Context) {
		s.SendMessage(c)
	},
	"send_group_msg": func(s *httpServer, c *gin.Context) {
		s.SendGroupMessage(c)
	},
	"send_group_forward_msg": func(s *httpServer, c *gin.Context) {
		s.SendGroupForwardMessage(c)
	},
	"send_private_msg": func(s *httpServer, c *gin.Context) {
		s.SendPrivateMessage(c)
	},
	"delete_msg": func(s *httpServer, c *gin.Context) {
		s.DeleteMessage(c)
	},
	"set_friend_add_request": func(s *httpServer, c *gin.Context) {
		s.ProcessFriendRequest(c)
	},
	"set_group_add_request": func(s *httpServer, c *gin.Context) {
		s.ProcessGroupRequest(c)
	},
	"set_group_card": func(s *httpServer, c *gin.Context) {
		s.SetGroupCard(c)
	},
	"set_group_special_title": func(s *httpServer, c *gin.Context) {
		s.SetSpecialTitle(c)
	},
	"set_group_kick": func(s *httpServer, c *gin.Context) {
		s.SetGroupKick(c)
	},
	"set_group_ban": func(s *httpServer, c *gin.Context) {
		s.SetGroupBan(c)
	},
	"set_group_whole_ban": func(s *httpServer, c *gin.Context) {
		s.SetWholeBan(c)
	},
	"set_group_name": func(s *httpServer, c *gin.Context) {
		s.SetGroupName(c)
	},
	"set_group_admin": func(s *httpServer, c *gin.Context) {
		s.SetGroupAdmin(c)
	},
	"_send_group_notice": func(s *httpServer, c *gin.Context) {
		s.SendGroupNotice(c)
	},
	"set_group_leave": func(s *httpServer, c *gin.Context) {
		s.SetGroupLeave(c)
	},
	"get_image": func(s *httpServer, c *gin.Context) {
		s.GetImage(c)
	},
	"get_forward_msg": func(s *httpServer, c *gin.Context) {
		s.GetForwardMessage(c)
	},
	"get_group_msg": func(s *httpServer, c *gin.Context) {
		s.GetGroupMessage(c)
	},
	"get_group_honor_info": func(s *httpServer, c *gin.Context) {
		s.GetGroupHonorInfo(c)
	},
	"can_send_image": func(s *httpServer, c *gin.Context) {
		s.CanSendImage(c)
	},
	"can_send_record": func(s *httpServer, c *gin.Context) {
		s.CanSendRecord(c)
	},
	"get_status": func(s *httpServer, c *gin.Context) {
		s.GetStatus(c)
	},
	"get_version_info": func(s *httpServer, c *gin.Context) {
		s.GetVersionInfo(c)
	},
	"_get_vip_info": func(s *httpServer, c *gin.Context) {
		s.GetVipInfo(c)
	},
	"get_stranger_info": func(s *httpServer, c *gin.Context) {
		s.GetStrangerInfo(c)
	},
	"reload_event_filter": func(s *httpServer, c *gin.Context) {
		s.ReloadEventFilter(c)
	},
	"set_group_portrait": func(s *httpServer, c *gin.Context) {
		s.SetGroupPortrait(c)
	},
	".handle_quick_operation": func(s *httpServer, c *gin.Context) {
		s.HandleQuickOperation(c)
	},
	".ocr_image": func(s *httpServer, c *gin.Context) {
		s.OcrImage(c)
	},
	".get_word_slices": func(s *httpServer, c *gin.Context) {
		s.GetWordSlices(c)
	},
}

func (s *httpServer) ShutDown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Http.Shutdown(ctx); err != nil {
		log.Fatal("http Server Shutdown:", err)
	}
	select {
	case <-ctx.Done():
		log.Println("timeout of 5 seconds.")
	}
	log.Println("http Server exiting")
}
