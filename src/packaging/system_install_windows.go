package packaging

import windowspkg "github.com/QuesmaOrg/quesma-shipper/packaging/windows"

func PrepareSystemInstall(dir, recoveryFile string) error {
	return windowspkg.PrepareSystemInstall(dir, recoveryFile)
}

func ResumeSystemInstall(dir, recoveryFile string) error {
	return windowspkg.ResumeSystemInstall(dir, recoveryFile)
}

func PostInstallSystem() error { return windowspkg.PostInstallSystem() }
