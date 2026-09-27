from google.adk.agents.llm_agent import LlmAgent
from google.adk.tools.mcp_tool.mcp_toolset import MCPToolset, StdioServerParameters

# Agent configuration
AGENT_NAME = "cad_design_agent"
MODEL_NAME = "gemini-2.5-flash-lite"
# The freecad-mcp binary from the one-line installer; use its absolute path
# if it is not on PATH.
FREECAD_MCP = "freecad-mcp"

# Basic instruction
BASIC_PROMPT = "You are a CAD designer."

# Initialize agent
root_agent = LlmAgent(
    model=MODEL_NAME,
    name=AGENT_NAME,
    instruction=BASIC_PROMPT,
    tools=[
        MCPToolset(
            connection_params=StdioServerParameters(
                command=FREECAD_MCP,
                args=["mcp"]
            )
        )
    ]
)
