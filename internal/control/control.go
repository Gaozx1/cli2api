package control

// Services is the console application surface assembled by app.New.
type Services struct {
	Accounts  *Accounts
	Keys      *Keys
	Settings  *Settings
	Backup    *Backup
	Catalog   *Catalog
	Donations *Donations
}

func New(runtime Runtime) *Services {
	if runtime == nil {
		return nil
	}
	store := runtime.Store()
	settings := NewSettings(store)
	accountService := NewAccounts(runtime)
	return &Services{
		Accounts:  accountService,
		Keys:      NewKeys(store),
		Settings:  settings,
		Backup:    NewBackup(store),
		Donations: NewDonations(settings, accountService),
	}
}
