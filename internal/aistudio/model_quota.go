package aistudio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var quotaModelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,199}$`)

// QuotaModelID 校验并规范化官网额度查询使用的模型 ID。
func QuotaModelID(model string) (string, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if !quotaModelPattern.MatchString(model) {
		return "", fmt.Errorf("%w: 额度查询需要有效的模型 ID", ErrInvalidArgument)
	}
	return model, nil
}

// ModelQuotaMetric 仅包含官网已确认的额度字段；未返回的数值保持未知。
type ModelQuotaMetric struct {
	Name      string `json:"name"`
	Limit     *int64 `json:"limit"`
	Remaining *int64 `json:"remaining"`
}

// ModelQuotaForAccount 读取官网 GetModelQuota，不生成内容或用本地统计推算余量。
func (c *Client) ModelQuotaForAccount(ctx context.Context, accountID, model string) ([]ModelQuotaMetric, error) {
	model, err := QuotaModelID(model)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal([]string{"models/" + model})
	if err != nil {
		return nil, err
	}
	response, err := c.do(ctx, "GetModelQuota", accountID, "", body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("读取 GetModelQuota: %w", err)
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("GetModelQuota 响应超过大小上限")
	}
	return decodeModelQuota(raw)
}

func decodeModelQuota(raw []byte) ([]ModelQuotaMetric, error) {
	value, err := decodeJSONValue(raw)
	if err != nil {
		return nil, withMethod(err, "GetModelQuota")
	}
	root, err := rawArray(value, "$", value)
	if err != nil {
		return nil, withMethod(err, "GetModelQuota")
	}
	metrics := make([]ModelQuotaMetric, 0)
	if isJSONNull(rawAt(root, 0)) {
		return metrics, nil
	}
	items, err := rawArray(root[0], "$[0]", value)
	if err != nil {
		return nil, withMethod(err, "GetModelQuota")
	}
	for index, item := range items {
		path := fmt.Sprintf("$[0][%d]", index)
		fields, err := rawArray(item, path, value)
		if err != nil {
			return nil, withMethod(err, "GetModelQuota")
		}
		name, err := rawString(rawAt(fields, 0), path+"[0]", value)
		if err != nil || strings.TrimSpace(name) == "" {
			if err == nil {
				err = &ProtocolEvidenceError{Path: path + "[0]", Detail: "额度指标名称不能为空", Raw: value}
			}
			return nil, withMethod(err, "GetModelQuota")
		}
		metric := ModelQuotaMetric{Name: name}
		for offset, target := range []**int64{&metric.Limit, &metric.Remaining} {
			field := rawAt(fields, offset+1)
			if isJSONNull(field) {
				continue
			}
			number, err := rawInt64(field, fmt.Sprintf("%s[%d]", path, offset+1), value)
			if err != nil || number < 0 {
				if err == nil {
					err = &ProtocolEvidenceError{Path: path, Detail: "额度不能为负数", Raw: value}
				}
				return nil, withMethod(err, "GetModelQuota")
			}
			*target = &number
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}
