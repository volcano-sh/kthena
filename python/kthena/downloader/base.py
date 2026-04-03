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

import os
import threading
from abc import ABC, abstractmethod
from typing import Optional
from typing import Tuple
from urllib.parse import urlparse

from kthena.downloader.lock import LockManager
from kthena.downloader.logger import setup_logger

logger = setup_logger()


def parse_bucket_from_model_url(url: str, scheme: str) -> Tuple[str, str]:
    result = urlparse(url, scheme=scheme)
    bucket_name = result.netloc
    bucket_path = result.path.lstrip("/")
    return bucket_name, bucket_path


class ModelDownloader(ABC):
    def __init__(self):
        self.lock_manager: Optional[LockManager] = None
        self.stop_event = threading.Event()

    @abstractmethod
    def download(self, output_dir: str):
        pass

    def download_model(self, output_dir: str):
        os.makedirs(output_dir, exist_ok=True)
        lock_path = os.path.join(output_dir, ".lock")
        self.lock_manager = LockManager(lock_path, timeout=15)
        while True:
            try:
                if self.lock_manager.try_acquire():
                    try:
                        logger.info(f"Acquired lock successfully. Starting download to {output_dir}")
                        self.download(output_dir)
                        break
                    except Exception as e:
                        logger.error(f"Error during model download: {e}")
                        raise
                    finally:
                        self.lock_manager.release()
                else:
                    logger.info("Failed to acquire lock. Waiting for the lock to be released.")
                    self.stop_event.wait(timeout=5)
            except Exception as e:
                logger.error(f"Unexpected error in download_model: {e}")
                if self.lock_manager:
                    self.lock_manager.release()
                raise


def get_downloader(source: str, config: dict, max_workers: int = 8) -> ModelDownloader:
    try:
        if source.startswith("s3://") or source.startswith("obs://"):
            from kthena.downloader.s3 import S3Downloader

            return S3Downloader(
                model_uri=source,
                access_key=config.get("access_key"),
                secret_key=config.get("secret_key"),
                endpoint=config.get("endpoint"),
            )
        elif source.startswith("pvc://"):
            from kthena.downloader.pvc import PVCDownloader

            return PVCDownloader(source_path=source)
        elif source.startswith("ms://"):
            from kthena.downloader.modelscope_downloader import ModelScopeDownloader

            return ModelScopeDownloader(
                model_uri=source.removeprefix("ms://"),
                ms_token=config.get("ms_token"),
                ms_revision=config.get("ms_revision"),
                max_workers=max_workers,
            )
        else:
            from kthena.downloader.huggingface import HuggingFaceDownloader

            return HuggingFaceDownloader(
                model_uri=source.removeprefix("hf://"),
                hf_token=config.get("hf_token"),
                hf_endpoint=config.get("hf_endpoint"),
                hf_revision=config.get("hf_revision"),
                max_workers=max_workers,
            )

    except ImportError as e:
        logger.error(f"Failed to initialize downloader: {e}")
        raise RuntimeError(f"Failed to initialize downloader: {e}")
