package server

import (
	"github.com/rs/zerolog"
)

type BaseRequest struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		NodeID   string `json:"node_id"`
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Private bool `json:"private"`
	} `json:"repository"`
}

func (req *BaseRequest) IsValid(logger *zerolog.Logger) bool {
	if req.Installation.ID == 0 {
		logger.Debug().Msg("no installation.id present in request")
		return false
	}
	if req.Repository.NodeID == "" {
		logger.Debug().Msg("no repository.node_id present in request")
		return false
	}
	if req.Repository.FullName == "" {
		logger.Debug().Msg("no repository.full_name present in request")
		return false
	}
	if req.Repository.Name == "" {
		logger.Debug().Msg("no repository.name present in request")
		return false
	}
	if req.Repository.NodeID == "" {
		logger.Debug().Msg("no repository.node_id present in request")
		return false
	}
	if req.Repository.Owner.Login == "" {
		logger.Debug().Msg("no repository.owner.login present in request")
		return false
	}
	return true
}
