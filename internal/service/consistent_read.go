package service

func (s *Service) storeGeneration() uint64 {
	if s.store == nil {
		return 0
	}
	return s.store.Generation()
}
