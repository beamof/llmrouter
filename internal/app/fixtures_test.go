package app

// wbSSEFixture 一段带思考、正文与工具调用的上游 chat 分片流（e2e 用例共用）
const wbSSEFixture = "data: " + `{"choices":[{"delta":{"reasoning_content":"思考"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"content":"你"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"content":"好"}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"get_time","arguments":"{\"tz\":"}}]}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]}}]}` + "\n\n" +
	"data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: " + `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":5}}}` + "\n\n" +
	"data: [DONE]\n\n"
