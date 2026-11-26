package sub2api

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel/newapi"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

type Adaptor struct {
	newapi.Adaptor
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	if request != nil && dto.IsKimiK31Model(request.Model) && request.TTL != nil {
		if len(request.CacheControl) == 0 || string(request.CacheControl) == "null" {
			cacheControl, err := common.Marshal(gin.H{"type": "ephemeral", "ttl": *request.TTL})
			if err != nil {
				return nil, err
			}
			request.CacheControl = cacheControl
		}
		request.TTL = nil
	}
	return a.Adaptor.ConvertClaudeRequest(c, info, request)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
