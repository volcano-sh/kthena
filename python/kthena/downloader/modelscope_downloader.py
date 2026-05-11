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

from modelscope import snapshot_download

from kthena.downloader.base import ModelDownloader
from kthena.downloader.logger import setup_logger

logger = setup_logger()


class ModelScopeDownloader(ModelDownloader):
    def __init__(self, model_uri: str, ms_revision: str = None, ms_token: str = None, max_workers: int = 8):
        super().__init__()
        self.model_uri = model_uri
        self.ms_revision = ms_revision
        self.ms_token = ms_token
        self.max_workers = max_workers

    def download(self, output_dir: str):
        logger.info(f"Downloading model from ModelScope: {self.model_uri}")
        try:
            snapshot_download(
                model_id=self.model_uri,
                revision=self.ms_revision,
                local_dir=output_dir,
                local_files_only=False,
                token=self.ms_token,
                max_workers=self.max_workers,
            )
        except Exception as e:
            logger.error(f"Error downloading model '{self.model_uri}': {e}")
            raise
