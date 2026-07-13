# Copyright The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


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
