from pydantic import BaseModel


class EncodeRequest(BaseModel):
    model_server_id: str
    text: str
    return_tokens: bool = False


class LoadRequest(BaseModel):
    model_server_id: str
    model_repo_id: str
    modelrevision: str | None = None


class UnLoadRequest(BaseModel):
    model_server_id: str
