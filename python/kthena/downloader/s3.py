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
import subprocess

from kthena.downloader.base import ModelDownloader, parse_bucket_from_model_url
from kthena.downloader.logger import setup_logger

logger = setup_logger()


class S3Downloader(ModelDownloader):
    def __init__(self, model_uri: str, access_key: str = None, secret_key: str = None, endpoint: str = None):
        super().__init__()
        self.access_key = access_key
        self.secret_key = secret_key
        self.endpoint = endpoint
        self.model_uri = model_uri

    def _prepare_environment(self):
        env = os.environ.copy()
        if self.access_key:
            env['AWS_ACCESS_KEY_ID'] = self.access_key
        if self.secret_key:
            env['AWS_SECRET_ACCESS_KEY'] = self.secret_key
        if self.endpoint:
            env['AWS_ENDPOINT_URL'] = self.endpoint
        return env

    @staticmethod
    def _build_sync_command(source, destination):
        cmd = ['aws', 's3', 'sync', source, destination]
        return cmd

    @staticmethod
    def _execute_command(cmd, env):
        process = subprocess.Popen(
            cmd,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1
        )

        while True:
            output = process.stdout.readline()
            if output == '' and process.poll() is not None:
                break
            if output:
                logger.info(output.strip())

        for error in process.stderr.readlines():
            logger.error(error.strip())

        return_code = process.poll()
        if return_code != 0:
            logger.error(f"AWS CLI command exited with status: {return_code}")
            raise Exception(f"AWS S3 sync command failed with exit code: {return_code}")

    def download(self, output_dir: str):
        logger.info(f"Syncing: {self.model_uri} to {output_dir}")

        if not self.access_key or not self.secret_key:
            raise ValueError("Missing credentials")

        os.makedirs(output_dir, exist_ok=True)
        bucket_name, bucket_path = parse_bucket_from_model_url(self.model_uri, "s3")
        if bucket_path:
            source = f"s3://{bucket_name}/{bucket_path}"
        else:
            source = f"s3://{bucket_name}"

        env = self._prepare_environment()
        cmd = self._build_sync_command(source, output_dir)

        try:
            self._execute_command(cmd, env)
            logger.info(f"Sync completed: {self.model_uri} -> {output_dir}")
        except Exception as e:
            logger.error(f"Error executing AWS CLI command: {str(e)}")
            raise
