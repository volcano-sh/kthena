# Development Setup

This document helps you get started developing code for Kthena. If you encounter any issues while following this guide, please take a few minutes to update this file.

Kthena components have only a few external dependencies that you need to set up before building and running the code.

## Setting up Go

All Kthena components are written in the [Go](https://golang.org) programming language. To build, you'll need a Go development environment. If you haven't set up a Go development environment, please follow [these instructions](https://golang.org/doc/install) to install the Go tools.

Kthena currently builds with Go 1.24.0.

## Setting up Docker

Kthena uses a Docker build system for creating and publishing Docker images. To leverage that you will need:

- **Docker platform:** To download and install Docker, follow [these instructions](https://docs.docker.com/get-docker/).
- **Container registry:** GitHub provides container image services (GitHub Container Registry). You can access them directly using your GitHub account. Alternatively, you can push built Kthena images to your private repository.

## Setting up Kubernetes

We require Kubernetes version 1.28 or higher with CRD support.

If you aren't sure which Kubernetes platform is right for you, see [Picking the Right Solution](https://kubernetes.io/docs/setup/).

- [Installing Kubernetes with Minikube](https://kubernetes.io/docs/setup/learning-environment/minikube/)
- [Installing Kubernetes with kops](https://kubernetes.io/docs/setup/production-environment/tools/kops/)
- [Installing Kubernetes with kind](https://kind.sigs.k8s.io/)

## Setting up a personal access token

This step is only necessary for core contributors who need to push changes to the main repositories. You can make pull requests without two-factor authentication, but the additional security is recommended for everyone.

To be part of the Volcano organization, we require two-factor authentication, and you must set up a personal access token to enable push via HTTPS. Please follow [these instructions](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens) for how to create a token.

Alternatively, you can [add your SSH keys](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/adding-a-new-ssh-key-to-your-github-account).
